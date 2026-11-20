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
WITHDRAWAL_MODES = ("funded-withdrawal", "funded-withdrawal-restart", "funded-withdrawal-publish-mark", "funded-withdrawal-race")
ROLES = ("RUNTIME", "NEXT_RUNTIME", "SWITCH", "RELAY")


def provision_funded(container, env, mode):
    roundtrip = mode != "funded-forward"
    working = mode in ("funded-working", *WITHDRAWAL_MODES)
    tenant = "synthetic-go-funded-working" if working else "synthetic-funded-cancel-fill"
    tenants = (tenant,) if roundtrip else ("synthetic-go-funded-forward", "synthetic-go-funded-mismatch")
    fixture.sql(container, """
        GRANT SELECT ON capital_balances,capital_commitments,capital_members,orders,
            synthetic_oms_commands,synthetic_oms_observations,outbox TO kanz_synthetic_oms_proof;
    """, fixture.DB)
    for tenant in tenants:
        fixture.sql(container, f"""
            BEGIN; SET LOCAL app.tenant_id='{tenant}';
            INSERT INTO capital_activation(enabled,activated_at) VALUES(true,clock_timestamp());
            INSERT INTO capital_source_health DEFAULT VALUES;
            INSERT INTO capital_balances(portfolio_id,currency,revision,coverage_complete,total,observed_at_ns)
                SELECT 'synthetic-portfolio',currency,1,true,opening,(extract(epoch FROM clock_timestamp())*1000000000)::bigint
                FROM (VALUES ('USD','1000'),('BNB','8')) AS cash(currency,opening);
            COMMIT;
        """, fixture.DB)
    if roundtrip:
        for migration in ("cancellation.sql", "settlement.sql", "go_writer.sql"):
            path = fixture.ROOT / "rust/services/synthetic_oms" / migration
            fixture.sql(container, path.read_text(encoding="utf-8"), fixture.DB)
        if mode in WITHDRAWAL_MODES:
            path = fixture.ROOT / "rust/services/synthetic_oms/go_cancel.sql"
            fixture.sql(container, path.read_text(encoding="utf-8"), fixture.DB)
            fixture.sql(container, "GRANT SELECT (order_id,fill) ON order_executions TO kanz_synthetic_oms_proof;", fixture.DB)
        role = "kanz_synthetic_oms_go_working_runtime" if working else "kanz_synthetic_oms_go_funded_runtime"
        fixture.sql(container, f"""
            GRANT {role} TO kanz_synthetic_oms_runtime_g3;
            GRANT SELECT, UPDATE(published_at,attempts,last_error) ON outbox TO kanz_synthetic_oms_runtime_g3;
        """, fixture.DB)
        if working:
            fixture.sql(container, """
                GRANT SELECT (order_id,fill_id,raw_fill_id) ON order_fills TO kanz_synthetic_oms_runtime_g3;
                GRANT SELECT (order_id,fill) ON order_executions TO kanz_synthetic_oms_runtime_g3;
            """, fixture.DB)
        fixture.grant_cash_projection(container, fixture.DB)
        fixture.provision_accounting(container, env)


def run(stage, native, mode):
    roundtrip = mode in ("roundtrip", "funded-roundtrip", "funded-working", *WITHDRAWAL_MODES)
    funded = mode in ("funded-forward", "funded-roundtrip", "funded-working", *WITHDRAWAL_MODES)
    targets = ["services/api_gateway/cmd/api_gateway"]
    if mode == "lineage":
        targets.append("services/oms/internal/order")
    elif roundtrip:
        targets.append("services/oms/cmd/oms")
        if funded:
            targets.append("services/accounting/internal/consume")
    fixture.build(stage, targets, tags="synthetic_handoff" if roundtrip else None)
    roles = ROLES
    if mode != "lineage":
        roles += ("CONTROL", "NEXT_ADAPTER")
        subprocess.run(["go", "build", "-mod=readonly", "-p", "1", "-tags", "synthetic_handoff", "-o", stage / "go-admit",
                        "./services/oms/tools/syntheticadmission"], cwd=fixture.ROOT / "kanz", check=True,
                       env=dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOWORK="off"))
    if roundtrip:
        roles += ("RECOVERY_RUNTIME", "VENUE")
    if funded:
        roles += ("FUNDING", "PROOF")
        if roundtrip:
            roles += ("CASH",)
    staged_native = stage / "native"
    shutil.copyfile(native, staged_native)
    staged_native.chmod(0o755)
    with staged_native.open("rb") as f:
        print("Rust owner SHA-256", hashlib.file_digest(f, "sha256").hexdigest(), flush=True)

    # Migration access stays on the Unix socket. Only the selected independent
    # logins need loopback access to test a retained order and attempt a switch.
    hba = stage / "pg_hba.conf"
    logins = ",".join(fixture.ROLES[name] for name in roles)
    hba.write_text("local all postgres peer\nlocal all all reject\n"
                   f"host {fixture.DB} {logins} 127.0.0.1/32 scram-sha-256\n"
                   f"host {fixture.BOOK_DB} {fixture.BOOK_ROLE} 127.0.0.1/32 scram-sha-256\n"
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
        names = [f"SYNTHETIC_OMS_{role}_URL" for role in roles]
        env = {name: env[name] for name in names}
        env["KANZ_SYNTHETIC_OMS_BIN"] = "/fixture/native"
        test = TEST
        if mode != "lineage":
            for migration in ("funding.sql", "go_admission.sql"):
                fixture.sql(container, (fixture.ROOT / "rust/services/synthetic_oms" / migration).read_text(encoding="utf-8"), fixture.DB)
        if funded:
            provision_funded(container, env, mode)
            if mode == "funded-withdrawal-race":
                env["KANZ_SYNTHETIC_GO_FUNDED_WITHDRAWAL_RACE"] = "1"
                test = "TestSyntheticFundedWithdrawalRaceGoRustGo"
            elif mode == "funded-withdrawal-publish-mark":
                env["KANZ_SYNTHETIC_GO_FUNDED_WITHDRAWAL_PUBLISH_MARK"] = "1"
                test = "TestSyntheticFundedWithdrawalPublishMarkGoRustGo"
            elif mode == "funded-withdrawal-restart":
                env["KANZ_SYNTHETIC_GO_FUNDED_WITHDRAWAL_RESTART"] = "1"
                test = "TestSyntheticFundedWithdrawalRestartGoRustGo"
            elif mode == "funded-withdrawal":
                env["KANZ_SYNTHETIC_GO_FUNDED_WITHDRAWAL"] = "1"
                test = "TestSyntheticFundedWithdrawalGoRustGo"
            elif mode == "funded-working":
                env["KANZ_SYNTHETIC_GO_FUNDED_WORKING"] = "1"
                test = "TestSyntheticFundedWorkingGoRustGo"
            elif roundtrip:
                env["KANZ_SYNTHETIC_GO_FUNDED_ROUNDTRIP"] = "1"
                test = "TestSyntheticFundedGoRustGoRoundTrip"
            else:
                env["KANZ_SYNTHETIC_GO_FUNDED_FORWARD"] = "1"
                test = "TestSyntheticGoFundedForward"
        elif mode == "roundtrip":
            gate = fixture.ROOT / "rust/services/synthetic_oms/go_writer.sql"
            fixture.sql(container, gate.read_text(encoding="utf-8"), fixture.DB)
            fixture.sql(container, """
                GRANT kanz_synthetic_oms_go_runtime TO kanz_synthetic_oms_runtime_g3;
                GRANT SELECT, UPDATE(published_at,attempts,last_error) ON outbox TO kanz_synthetic_oms_runtime_g3;
                BEGIN; SET LOCAL app.tenant_id='synthetic-go-roundtrip';
                INSERT INTO capital_activation(enabled,activated_at) VALUES(false,NULL);
                COMMIT;
            """, fixture.DB)
            env["KANZ_SYNTHETIC_GO_ROUNDTRIP"] = "1"
            test = "TestSyntheticGoRustGoRoundTrip"
        elif mode == "forward":
            for tenant, active in (("synthetic-go-armed", "true"), ("synthetic-go-legacy-evidence", "false"),
                                   ("synthetic-go-expired", "false"), ("synthetic-go-forward", "false")):
                fixture.sql(container, f"BEGIN; SET LOCAL app.tenant_id='{tenant}'; INSERT INTO capital_activation(enabled,activated_at) "
                                       f"VALUES({active},CASE WHEN {active} THEN clock_timestamp() END); COMMIT;", fixture.DB)
            env["KANZ_SYNTHETIC_GO_FORWARD"] = "1"
            test = "TestSyntheticGoOriginForward"
        else:
            env["KANZ_SYNTHETIC_GO_LINEAGE"] = "1"
        cmd = ["docker", "run", "--pull=never", "--rm", "--platform", "linux/amd64", "--read-only",
               "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532",
               "--network", "container:" + container, "--tmpfs", "/tmp:exec,mode=1777",
               "-v", f"{stage}:/fixture:ro", *[arg for name in env for arg in ("-e", name)],
               "--entrypoint", "/fixture/api_gateway", fixture.RUNTIME,
               "-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=1m"]
        log = fixture.run_fixture_tests(cmd, [test], env, password)
        if mode == "forward":
            fixture.require_cases(log, test, ("synthetic-go-armed", "synthetic-go-legacy-evidence", "synthetic-go-missing-posture", "synthetic-go-expired", "synthetic-go-forward"))
        elif mode == "funded-forward":
            fixture.require_cases(log, test, ("synthetic-go-funded-mismatch", "synthetic-go-funded-forward"))
        print(log, end="", flush=True)
    finally:
        subprocess.run(["docker", "stop", "-t", "1", container], stdout=subprocess.DEVNULL, check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--native", type=Path, required=True, help="Linux amd64 kanz-synthetic-oms executable")
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--forward", action="store_const", dest="mode", const="forward",
                       help="qualify authenticated unfunded Go admission and transfer to Rust")
    group.add_argument("--roundtrip", action="store_const", dest="mode", const="roundtrip",
                       help="admit one unfunded order through Go and recover it through Rust and Go")
    group.add_argument("--funded-forward", action="store_const", dest="mode", const="funded-forward",
                       help="qualify atomic Go admission and transfer of a funded order")
    group.add_argument("--funded-roundtrip", action="store_const", dest="mode", const="funded-roundtrip",
                       help="recover a single funded partial execution and cancellation across Go, Rust and Go")
    group.add_argument("--funded-working", action="store_const", dest="mode", const="funded-working",
                       help="recover a single open funded order after its partial execution")
    group.add_argument("--funded-withdrawal", action="store_const", dest="mode", const="funded-withdrawal",
                       help="withdraw the working remainder after the funded Go handback")
    group.add_argument("--funded-withdrawal-restart", action="store_const", dest="mode", const="funded-withdrawal-restart",
                       help="recover a funded withdrawal across two process exits and publish its retained terminal fact")
    group.add_argument("--funded-withdrawal-publish-mark", action="store_const", dest="mode", const="funded-withdrawal-publish-mark",
                       help="recover a withdrawal after broker publication and before its SQL outbox acknowledgement")
    group.add_argument("--funded-withdrawal-race", action="store_const", dest="mode", const="funded-withdrawal-race",
                       help="deliver one signed withdrawal to two Go processes while its XSIM cancellation is in flight")
    parser.set_defaults(mode="lineage")
    args = parser.parse_args()
    if not args.native.is_file():
        parser.error("--native requires a built Linux executable")
    with tempfile.TemporaryDirectory(prefix="kanz-go-writer-lineage-") as directory:
        run(Path(directory), args.native, args.mode)
