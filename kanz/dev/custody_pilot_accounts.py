"""Exercise the custody account onboarding commands in the disposable pilot."""

import http.client
import json
import secrets
import socket
import subprocess
import time


def verify_startup(binary, env, reader_url, admitted):
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    service_env = dict(env, ACCOUNTING_DATABASE_URL=reader_url, ACCOUNTING_TENANT="synthetic-custody-pilot",
                       ACCOUNTING_CUSTODY_PAIRS="PF1:CUST-A", ACCOUNTING_CUSTODY_ACCOUNTS="PF1:CUST-A:sandbox-account",
                       ACCOUNTING_LISTEN=f"127.0.0.1:{port}", ACCOUNTING_METRICS_LISTEN="127.0.0.1:0")
    with subprocess.Popen([binary], env=service_env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as service:
        try:
            if not admitted:
                output, _ = service.communicate(timeout=10)
                if service.returncode != 2 or b"custody account assignment admission failed" not in output:
                    raise RuntimeError("Accounting did not refuse the missing independent account assignment at startup.")
                return
            deadline = time.monotonic() + 10
            while service.poll() is None and time.monotonic() < deadline:
                connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
                try:
                    connection.request("GET", "/healthz")
                    if connection.getresponse().status == 200 and service.poll() is None:
                        return
                except (OSError, http.client.HTTPException):
                    time.sleep(0.1)
                finally:
                    connection.close()
            raise RuntimeError("Accounting did not become ready with the proven synthetic assignment.")
        finally:
            if service.poll() is None:
                service.terminate()
            try:
                service.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                service.kill()
                service.communicate()


def enroll_accounts(sql, database, endpoint, reader_url, binaries, env, fixture, *, check_startup):
    # The fixture administrator provisions independent public trust anchors. Only
    # the two issuer logins submit signed evidence through the actual command.
    sql(database, b"""
        INSERT INTO custody_assignment_readers (login_role,tenant_id)
        VALUES ('pilot_check','synthetic-custody-pilot');
        GRANT SELECT ON custody_assignment_readers,custody_proof_issuers,custody_proof_scopes,
            custody_account_proofs,custody_account_assignments TO pilot_check;
    """)
    if check_startup:
        verify_startup(binaries["service"], env, reader_url, False)
    issuers = {row["kind"]: row for row in json.loads((fixture / "issuers.json").read_bytes())}
    scopes = {row["login_role"]: row for row in json.loads((fixture / "scopes.json").read_bytes())}
    for kind, role, table in (("venue_identity", "pilot_venue_proof", "custody_account_proofs"),
                              ("custody_assignment", "pilot_custody_assignment", "custody_account_assignments")):
        issuer = issuers[kind]
        if issuer["login_role"] != role or issuer["tenant_id"] != "synthetic-custody-pilot":
            raise RuntimeError("Unexpected issuer identity in the synthetic custody fixture.")
        scope = scopes[role]
        password = secrets.token_hex(24)
        sql("postgres", f"CREATE ROLE {role} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{password}'".encode())
        sql(database, b"""
            INSERT INTO custody_proof_issuers (login_role,kind,tenant_id,authority,public_key)
            VALUES (:'login',:'kind',:'tenant',:'authority',decode(:'pubkey','base64'));
            INSERT INTO custody_proof_scopes (login_role,tenant_id,venue,account_id,exchange_account_id,portfolio_id,custodian_id)
            VALUES (:'login',:'tenant',:'venue',:'account',:'exchange_id',:'portfolio',:'custodian');
        """, "-v", "login=" + role, "-v", "kind=" + kind, "-v", "tenant=" + issuer["tenant_id"],
            "-v", "authority=" + issuer["authority"], "-v", "pubkey=" + issuer["public_key"],
            "-v", "venue=" + scope["venue"], "-v", "account=" + scope["account_id"],
            "-v", "exchange_id=" + scope["exchange_account_id"], "-v", "portfolio=" + scope["portfolio_id"],
            "-v", "custodian=" + scope["custodian_id"])
        sql(database, f"""GRANT USAGE ON SCHEMA public TO {role};
            GRANT SELECT ON custody_assignment_readers,custody_proof_issuers,custody_proof_scopes,
                custody_account_proofs,custody_account_assignments TO {role};
            GRANT INSERT ON {table} TO {role};""".encode())
        issuer_env = dict(env, ACCOUNTING_DATABASE_URL=f"postgres://{role}:{password}@{endpoint}/{database}?sslmode=disable",
                          ACCOUNTING_TENANT=issuer["tenant_id"])
        for _ in range(2):
            import_evidence(binaries["proof"], issuer_env, fixture, kind, kind)
        if check_startup:
            verify_startup(binaries["service"], env, reader_url, kind == "custody_assignment")
        if kind == "custody_assignment" and check_startup:
            def check_withdrawal():
                for name, admitted in (("custody_assignment_withdrawn", False),
                                       ("custody_assignment_reinstated", True)):
                    import_evidence(binaries["proof"], issuer_env, fixture, "custody_assignment", name)
                    verify_startup(binaries["service"], env, reader_url, admitted)
                return {"without_evidence": "refused", "identity_only": "refused", "assigned": "ready",
                        "withdrawn": "refused", "reinstated": "ready"}
            return check_withdrawal
    return None


def import_evidence(binary, env, fixture, kind, name):
    call = [binary, "-kind", kind, "-evidence", fixture / (name + ".json"),
            "-signature", fixture / (name + ".sig")]
    result = subprocess.run(call, env=env, capture_output=True, timeout=10)
    if result.returncode:
        raise RuntimeError("The synthetic custody proof importer refused " + name + ".")
