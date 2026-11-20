"""Run the OMS checks against disposable Linux processes with no network egress."""
import argparse
import hashlib
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
PG = "postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9"
RUNTIME = "ghcr.io/eighred/base/rust@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f"
# Official Microsoft Playwright v1.65.1-noble, Linux amd64.
BROWSER_IMAGE = "mcr.microsoft.com/playwright@sha256:766137e2d9caad4744187e53cdc89320ba6f2eb57cff097374020944b44dd0a9"
BROWSER_VERSION = "1.65.1"
BLOTTER = "TestPersistedNativeOrderBlotter"
DB = "kanz_synthetic_oms"
BOOK_DB = "kanz_synthetic_accounting"
BOOK_ROLE = "kanz_synthetic_accounting"
HISTORY_ROLE = "kanz_synthetic_cash_history"
REBUILD_DB = "kanz_synthetic_cash_rebuild"
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


def require_cases(log, root, names):
    for name in names:
        case = re.escape(f"{root}/{name}")
        if len(re.findall(rf"^\s*--- PASS: {case} ", log, re.MULTILINE)) != 1:
            raise RuntimeError(f"missing synthetic proof: {root}/{name}")


ROLES = {
    "CONTROL": "kanz_synthetic_oms_control",
    "FUNDING": "kanz_synthetic_oms_funding",
    "CASH": "kanz_synthetic_oms_cash",
    "PROOF": "kanz_synthetic_oms_proof",
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


def provision_accounting(container, env):
    password = secrets.token_hex(24)
    sql(container, f"CREATE ROLE {BOOK_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{password}';\n"
                   f"CREATE DATABASE {BOOK_DB};")
    for migration in sorted((ROOT / "kanz/services/accounting/migrations").glob("*.sql")):
        sql(container, migration.read_text(encoding="utf-8"), BOOK_DB)
    sql(container, f"""
        GRANT SELECT ON ledger_entries,ledger_snapshots,ledger_heads,ledger_append_positions,cash_commands,
            execution_fee_revisions,cash_commit_heads,cash_commit_debits,cash_commit_history,outbox TO {BOOK_ROLE};
        GRANT INSERT ON ledger_entries,ledger_heads,ledger_append_positions,cash_commit_heads,cash_commit_debits,
            cash_commit_history,outbox TO {BOOK_ROLE};
        GRANT UPDATE(position) ON ledger_heads TO {BOOK_ROLE};
        GRANT UPDATE(revision,observed_at_ns) ON cash_commit_heads TO {BOOK_ROLE};
        GRANT UPDATE(debit) ON cash_commit_debits TO {BOOK_ROLE};
        GRANT UPDATE(published_at,attempts,last_error) ON outbox TO {BOOK_ROLE};
        GRANT USAGE ON SEQUENCE outbox_id_seq TO {BOOK_ROLE};
    """, BOOK_DB)
    env["SYNTHETIC_BOOK_URL"] = f"postgres://{BOOK_ROLE}:{password}@127.0.0.1:5432/{BOOK_DB}?sslmode=disable"
    env["SYNTHETIC_BOOK_BIN"] = "/fixture/consume"


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
          DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.tenant_id='synthetic-tenant')
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
        raise RuntimeError(re.sub(r"FUNDED_BLOTTER_RECEIPT [^\n]*", "[synthetic receipt withheld]", log))
    require_tests(log, names)
    return log


def grant_cash_projection(container, database):
    sql(container, """
        BEGIN;
        GRANT SELECT,INSERT,UPDATE(fault_digest) ON capital_source_health TO kanz_synthetic_oms_cash;
        GRANT SELECT,INSERT(portfolio_id,currency),UPDATE(reserved,revision,total,observed_at_ns,coverage_complete,conflicted,gap_revision)
            ON capital_balances TO kanz_synthetic_oms_cash;
        GRANT SELECT,INSERT(portfolio_id,currency,revision,digest) ON capital_cash_events TO kanz_synthetic_oms_cash;
        GRANT SELECT,UPDATE(booked_debit) ON capital_commitments,capital_members TO kanz_synthetic_oms_cash;
        COMMIT;
    """, database)


def provision_upgrade(container, env):
    password = secrets.token_hex(24)
    sql(container, f"CREATE ROLE {HISTORY_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{password}';")
    sql(container, f"GRANT SELECT ON cash_commit_heads,cash_commit_history TO {HISTORY_ROLE};", BOOK_DB)
    env["SYNTHETIC_BOOK_HISTORY_URL"] = f"postgres://{HISTORY_ROLE}:{password}@127.0.0.1:5432/{BOOK_DB}?sslmode=disable"
    sql(container, f"CREATE DATABASE {REBUILD_DB};")
    for migration in sorted((ROOT / "kanz/services/oms/migrations").glob("*.sql")):
        sql(container, migration.read_text(encoding="utf-8"), REBUILD_DB)
    grant_cash_projection(container, REBUILD_DB)
    env["SYNTHETIC_REBUILD_URL"] = env["SYNTHETIC_OMS_CASH_URL"].replace(f"/{DB}?", f"/{REBUILD_DB}?")
    env["SYNTHETIC_PRIOR_BOOK_BIN"] = "/fixture/prior-accounting"


def prepare_browser():
    web = ROOT / "web"
    for package in ("playwright", "playwright-core"):
        manifest = web / "node_modules" / package / "package.json"
        if not manifest.is_file() or json.loads(manifest.read_text(encoding="utf-8"))["version"] != BROWSER_VERSION:
            raise RuntimeError("Install the locked web dependencies with npm ci before using --browser.")
    npm = shutil.which("npm.cmd" if os.name == "nt" else "npm")
    if not npm:
        raise RuntimeError("The Node toolchain in web/.nvmrc is required.")
    subprocess.run([npm, "run", "build"], cwd=web, check=True)
    if subprocess.run(["docker", "image", "inspect", BROWSER_IMAGE],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        subprocess.run(["docker", "pull", BROWSER_IMAGE], check=True)


def funded_browser(stage, container, read_url, venue_log):
    receipts = re.findall(r"FUNDED_BLOTTER_RECEIPT (.+)$", venue_log, flags=re.MULTILINE)
    if len(receipts) != 1 or len(receipts[0]) >= 512 * 1024:
        raise RuntimeError("Expected one bounded terminal receipt from partial_cancel_and_book.")
    web = ROOT / "web"
    mounts = [(stage / "api_gateway", "/probe/gateway"), (stage / "server", "/probe/bff"),
              (web / "dist", "/probe/dist"), (web / "e2e/native-blotter.cjs", "/probe/blotter.cjs")]
    for package in ("playwright", "playwright-core"):
        mounts.append(((web / "node_modules" / package).resolve(), "/probe/node_modules/" + package))
    # Only the read-role DSN enters this disposable container. The signed gateway
    # fixture strips it before launching the BFF, which privately supplies the two browser sessions.
    name = "kanz-funded-blotter-" + secrets.token_hex(8)
    cmd = ["docker", "run", "--name", name, "--rm", "-i", "--init", "--pull=never", "--platform", "linux/amd64",
           "--network", "container:" + container, "--read-only", "--cap-drop", "ALL",
           "--security-opt", "no-new-privileges", "--user", "65532:65532", "--tmpfs", "/tmp:mode=1777",
           "--env", "HOME=/tmp", "--env", "PLAYWRIGHT_BROWSERS_PATH=/ms-playwright",
           "--env", "SYNTHETIC_OMS_READ_URL", "--env", "SYNTHETIC_BLOTTER_RECEIPT_STDIN=1",
           "--env", "SYNTHETIC_BLOTTER_BROWSER_PROBE=/probe/bff", "--env", "TEST_MFA_NODE=node",
           "--env", "TEST_MFA_PLAYWRIGHT=/probe/node_modules/playwright", "--env", "TEST_MFA_STATIC_DIR=/probe/dist",
           "--env", "TEST_NATIVE_BLOTTER_SCRIPT=/probe/blotter.cjs"]
    for source, destination in mounts:
        cmd.extend(["-v", f"{source}:{destination}:ro"])
    try:
        proc = subprocess.run([*cmd, "--entrypoint", "/probe/gateway", BROWSER_IMAGE,
                               "-test.run", "^" + BLOTTER + "$", "-test.v", "-test.count=1", "-test.timeout=2m"],
                              input=receipts[0], env=dict(os.environ, SYNTHETIC_OMS_READ_URL=read_url),
                              capture_output=True, text=True, timeout=150)
    except (subprocess.TimeoutExpired, KeyboardInterrupt):
        # The attached CLI may exit while its Chromium container is still alive.
        subprocess.run(["docker", "rm", "-f", name], capture_output=True, timeout=20)
        raise
    if proc.returncode:
        raise RuntimeError("Funded signed gateway/BFF/Chromium verification failed: " +
                           (proc.stdout + proc.stderr).replace(read_url, "[fixture connection]"))
    require_tests(proc.stdout, [BLOTTER])
    print("Chromium: confirmed XSIM cancellation, filled 0.625 and unfilled 0.625 of 1.25; "
          "authorized read, stale/refresh and foreign tenant refusal passed", flush=True)


def run(stage, native, prior_accounting=None, browser=False):
    packages = {path: list(names) for path, names in OWNERS.items()}
    if native:
        packages["services/api_gateway/cmd/api_gateway"].append("TestSyntheticOrderCommandLifecycle")
        shutil.copyfile(native, stage / "native")
        (stage / "native").chmod(0o755)
        with native.open("rb") as binary:
            print("Rust owner SHA-256", hashlib.file_digest(binary, "sha256").hexdigest(), flush=True)
    if prior_accounting:
        shutil.copyfile(prior_accounting, stage / "prior-accounting")
        (stage / "prior-accounting").chmod(0o755)
        with prior_accounting.open("rb") as binary:
            print("Prior accounting SHA-256", hashlib.file_digest(binary, "sha256").hexdigest(), flush=True)
    targets = list(packages)
    if native:
        targets.append("services/accounting/internal/consume")
    if browser:
        prepare_browser()
        targets.append("services/web_bff/internal/server")
    build(stage, targets)
    # Local peer auth is for migration setup only. All other local users and
    # all remote administrators are refused. Only the test user gets a SCRAM DSN.
    hba = stage / "pg_hba.conf"
    hba.write_text("local all postgres peer\nlocal all all reject\n"
                   f"host {DB} {','.join(['kanz_synthetic_test', *ROLES.values()])} 127.0.0.1/32 scram-sha-256\n"
                   f"host {BOOK_DB} {BOOK_ROLE},{HISTORY_ROLE} 127.0.0.1/32 scram-sha-256\n"
                   f"host {REBUILD_DB} {ROLES['CASH']} 127.0.0.1/32 scram-sha-256\n"
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
            provision_accounting(container, fixture_env)
            if prior_accounting:
                provision_upgrade(container, fixture_env)
            fixture_env["KANZ_SYNTHETIC_OMS_BIN"] = "/fixture/native"
        if browser:
            fixture_env["SYNTHETIC_BLOTTER_CAPTURE"] = "1"
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
                funding = ROOT / "rust/services/synthetic_oms/funding.sql"
                sql(container, funding.read_text(encoding="utf-8"), DB)
                fixture = ROOT / "kanz/dev/synthetic_funding_fixture.sql"
                sql(container, fixture.read_text(encoding="utf-8"), DB)
                funded = "TestSyntheticFundedOrderAdmission"
                funded_log = run_fixture_tests(cmd, [funded], selected_env, password)
                cases = ("ok", "ok/funded_dispatch_held", "short", "short-fee", "race", "rollback", "stale", "foreign")
                require_cases(funded_log, funded, cases)
                print(f"{package}: funded admission passed ({', '.join(cases)}) without skips", flush=True)
                cancellation = ROOT / "rust/services/synthetic_oms/cancellation.sql"
                sql(container, cancellation.read_text(encoding="utf-8"), DB)
                sql(container, """
                    GRANT SELECT ON public.synthetic_oms_funded_withdrawals TO kanz_synthetic_oms_proof;
                    DO $$ DECLARE actor name; BEGIN
                        FOREACH actor IN ARRAY ARRAY['kanz_synthetic_oms_runtime','kanz_synthetic_oms_adapter',
                                'kanz_synthetic_oms_control','kanz_synthetic_oms_funding'] LOOP
                            IF has_any_column_privilege(actor,'public.capital_balances','UPDATE')
                               OR has_any_column_privilege(actor,'public.capital_commitments','UPDATE')
                               OR has_any_column_privilege(actor,'public.synthetic_oms_funded_withdrawals','UPDATE')
                               OR has_any_column_privilege(actor,'public.synthetic_oms_cancellation_authorizations','UPDATE')
                               OR has_function_privilege(actor,'public.synthetic_oms_withdraw_funded()','EXECUTE')
                               OR has_any_column_privilege(actor,'public.synthetic_oms_cancellation_authorizations','INSERT')
                                  IS DISTINCT FROM (actor='kanz_synthetic_oms_control') THEN
                                RAISE EXCEPTION 'unexpected cancellation privileges for %',actor;
                            END IF;
                        END LOOP;
                    END $$;
                    BEGIN;
                    SET LOCAL ROLE kanz_synthetic_oms_proof;
                    SET LOCAL app.tenant_id='synthetic-funded-ok';
                    DO $$ BEGIN
                        IF (SELECT count(*) FROM public.orders WHERE status=1)<>1
                           OR (SELECT count(*) FROM public.outbox WHERE subject='order.order.accepted')<>1
                           OR EXISTS (SELECT FROM public.synthetic_oms_funded_withdrawals)
                           OR (SELECT count(*) FROM public.capital_balances
                               WHERE (currency,reserved) IN (('USD','252'),('BNB','1')))<>2 THEN
                            RAISE EXCEPTION 'cancellation installation changed a prior funded acceptance';
                        END IF;
                    END $$;
                    COMMIT;
                """, DB)
                terminal = "TestSyntheticFundedOrderCancellation"
                terminal_log = run_fixture_tests(cmd, [terminal], selected_env, password)
                terminal_cases = ("ok", "race", "rollback", "scope")
                require_cases(terminal_log, terminal, terminal_cases)
                print(f"{package}: funded cancellation passed ({', '.join(terminal_cases)}) without skips", flush=True)
                settlement = ROOT / "rust/services/synthetic_oms/settlement.sql"
                sql(container, settlement.read_text(encoding="utf-8"), DB)
                for tenant in ("synthetic-funded-venue", "synthetic-funded-venue-nanos", "synthetic-funded-venue-pair",
                               "synthetic-funded-before-send"):
                    sql(container, f"""
                        BEGIN;
                        SET LOCAL app.tenant_id='{tenant}';
                        INSERT INTO capital_activation(enabled,activated_at) VALUES(true,clock_timestamp());
                        INSERT INTO capital_source_health DEFAULT VALUES;
                        INSERT INTO capital_balances(portfolio_id,currency,revision,coverage_complete,total,observed_at_ns)
                            SELECT 'synthetic-portfolio',currency,1,true,opening,(extract(epoch FROM clock_timestamp())*1000000000)::bigint
                            FROM (VALUES ('USD','1000'),('BNB','8')) AS cash(currency,opening);
                        COMMIT;
                    """, DB)
                grant_cash_projection(container, DB)
                venue = "TestSyntheticFundedVenueLifecycle"
                venue_log = run_fixture_tests(cmd, [venue, "TestSyntheticAccountingCashClock"], selected_env, password)
                require_cases(venue_log, venue, ("partial_cancel_and_book", "submicrosecond_receipt", "same_microsecond_pair", "claim_before_venue_send"))
                if browser:
                    funded_browser(stage, container, fixture_env["SYNTHETIC_OMS_READ_URL"], venue_log)
                for line in venue_log.splitlines():
                    if "XSIM " in line and "synthetic_" in line:
                        print(line[line.index("XSIM "):], flush=True)
                print(f"{package}: funded XSIM partial fill, cancellation, journal and cash replay passed without skips", flush=True)
            log = run_fixture_tests(cmd, names, selected_env, password)
            if native and base == "api_gateway":
                require_cases(log, "TestSyntheticOrderCommandLifecycle",
                              ("armed_without_reservation", "unrelated_order_constraint", "fenced_restart", "unknown_across_handoff"))
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
    parser.add_argument("--prior-accounting", type=Path, help="also use a Linux accounting test binary built from 7077c75b for the historical tenant")
    parser.add_argument("--browser", action="store_true", help="read the actual funded terminal order through signed gateway, BFF and isolated Chromium")
    args = parser.parse_args()
    if args.browser and not args.native:
        parser.error("--browser requires --native")
    if args.prior_accounting and not args.native:
        parser.error("--prior-accounting requires --native")
    with tempfile.TemporaryDirectory(prefix="kanz-synthetic-orders-") as directory:
        run(Path(directory), args.native, args.prior_accounting, args.browser)
