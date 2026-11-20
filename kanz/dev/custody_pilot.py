#!/usr/bin/env python3
"""Run the synthetic signed-custody operator workflow on a private local fixture."""

import argparse
from decimal import Decimal
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

from custody_pilot_accounts import enroll_accounts
from custody_pitr import CustodyPITR

ROOT = Path(__file__).resolve().parents[2]
MODULE = ROOT / "kanz"
WEB = ROOT / "web"
CMD = MODULE / "services/accounting/cmd/custody_batch"
IMAGE = "postgres@sha256:aa6cb33cf4c19c765cefcf48d96176258800870a9a1e930ad126200f9d825139"
DATABASE = "kanz_custody_pilot"
PACKAGES = {
    "accounting": "./services/accounting/cmd/custody_batch",
    "assignment_test": "./services/accounting/internal/custody",
    "gateway": "./services/api_gateway/cmd/api_gateway",
    "bff": "./services/web_bff/internal/server",
}


def host_environment():
    # Runtime settings for local programs, excluding ambient service URLs,
    # bearer tokens, proxy credentials and database connection variables.
    names = (
        "PATH", "SystemRoot", "SystemDrive", "ComSpec", "PATHEXT", "TEMP", "TMP", "TMPDIR",
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
    for name, package in (("service", "accounting"), ("proof", "custody_proof"), ("source", "custody_batch")):
        binary = work / (name + (".exe" if os.name == "nt" else ""))
        subprocess.run(["go", "build", "-mod=readonly", "-p", "1", "-o", binary,
                        "./services/accounting/cmd/" + package], cwd=MODULE,
                       env=dict(build_env, GOWORK="off"), check=True)
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


def verify_observations(path, fixture, rerun=False, quantity_evidence=False):
    records = json.loads(path.read_bytes())
    revisions = [1, 1, 2] if rerun else [1, 2]
    if not isinstance(records, list) or len(records) != len(revisions):
        raise RuntimeError("Both signed statement revisions must have a retained comparison.")
    runs = set()
    for sequence, (revision, record) in enumerate(zip(revisions, records), 1):
        if not isinstance(record, dict) or not all(type(record.get(key)) is int for key in
                                                  ("revision", "outstanding_breaks", "retained_breaks", "completion_sequence")):
            raise RuntimeError("Expected an observed revision, break count and completion sequence.")
        raw = (fixture / f"statement-r{revision}.json").read_bytes()
        statement_id = f"custody-batch.v1.synthetic-source.r{revision}." + hashlib.sha256(raw).hexdigest()
        run_id = record.get("run_id")
        expected = {
            "revision": revision, "statement_id": statement_id, "run_id": run_id,
            "business_date": json.loads(raw)["statement"]["businessDate"].split("T")[0],
            "outcome": "breaks" if revision == 1 else "clean",
            "outstanding_breaks": (2 if quantity_evidence else 1) if revision == 1 else 0,
            "retained_breaks": 1 if revision == 1 else 0, "completion_sequence": sequence,
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


def verify_late_correction(path, fixture):
    observed = json.loads(path.read_bytes())
    download_path = path.with_suffix(".downloads.json")
    if download_path.stat().st_size > 128 * 1024:
        raise RuntimeError("Historical correction exceeds the pilot receipt bound.")
    downloads = json.loads(download_path.read_bytes())
    download = downloads["correction"]
    expected = {"tenant": "synthetic-custody-pilot", "portfolio": "PF1", "custodian": "CUST-A",
                "business_date": "2020-02-04", "following_date": "2020-02-05",
                "entry_id": "pilot-trade-7", "external_ref": "external-7", "instrument_id": "SYN", "agreed_quantity": "10.5"}
    for key, name, revision in (("original_statement", "statement-r1", 1), ("following_statement", "following-r1", 1),
                                ("corrected_statement", "statement-r2", 2)):
        digest = hashlib.sha256((fixture / (name + ".json")).read_bytes()).hexdigest()
        expected[key] = f"custody-batch.v1.synthetic-source.r{revision}.{digest}"
    runs = [observed.get(key) for key in ("original_run", "following_run", "correction_run")]
    if (any(observed.get(key) != value for key, value in expected.items())
            or not all(isinstance(run, str) and run for run in runs) or len(set(runs)) != 3
            or not re.fullmatch(r"q1:[0-9a-f]{64}", observed.get("break_id", ""))):
        raise RuntimeError("The three accounting observations do not identify the signed pilot history.")
    resolution = {"break_id": observed["break_id"], "detection_run_id": runs[0],
                  "entry_id": expected["entry_id"], "external_ref": expected["external_ref"],
                  "instrument_id": expected["instrument_id"], "agreed_quantity": {"coefficient": "105", "exponent": -1}}
    if (not isinstance(download, dict) or download.get("format") != "kanz.custody-comparison.v1"
            or download.get("lookup") != "retained_run" or download.get("outcome") != "historical_quantity_correction"
            or download.get("run_id") != runs[2] or download.get("statement_id") != expected["corrected_statement"]
            or download.get("signed_statement_sha256") != expected["corrected_statement"].rsplit(".", 1)[-1]
            or download.get("account_assignment_verified") is not True
            or download.get("historical_quantity_resolutions") != [resolution]
            or any(key in download for key in ("breaks", "execution_quantities", "transaction_pass", "reported_execution_figures"))):
        raise RuntimeError("Customer download does not match the applied historical quantity correction.")
    reported = downloads["reported"]
    signed_trade = {"external_ref": expected["external_ref"], "instrument_id": expected["instrument_id"],
                    "reported_price": "1", "reported_cash": "-9.5", "cash_currency": "USD"}
    for receipt, run, stmt, rows in zip(reported, runs[:2], ("original_statement", "following_statement"),
                                        ([signed_trade], []), strict=True):
        digest = expected[stmt].rsplit(".", 1)[-1]
        if (receipt.get("run_id") != run or receipt.get("statement_id") != expected[stmt]
                or receipt.get("signed_statement_sha256") != digest or receipt.get("account_assignment_verified") is not True
                or receipt.get("reported_execution_figures") != {
                    "basis": "signed_custodian_statement", "signed_statement_sha256": digest, "rows": rows}):
            raise RuntimeError("Downloaded execution figures differ from the original signed pilot statement.")
    return {"evidence": observed, "customer_download": download, "reported_execution_downloads": reported}


def verify_execution_economics(path, fixture):
    # Use the independently known booked fill and the bytes signed by the
    # synthetic custodian. Browser downloads are the observations under test.
    def amount(field):
        return Decimal(field["coefficient"]).scaleb(field.get("exponent", 0))

    downloads = path.with_suffix(".downloads.json")
    if downloads.stat().st_size > 256 * 1024:
        raise RuntimeError("Execution comparison downloads exceed the pilot bound.")
    records = json.loads(downloads.read_bytes())
    if len(records) != 4:
        raise RuntimeError("Four signed execution revisions must be recoverable.")
    runs = set()
    book = {"quantity": Decimal("10.5"), "price": Decimal("1"), "commission": Decimal("0.25"),
            "gross_cash": Decimal("-10.5"), "net_cash": Decimal("-10.75"), "settled_cash": Decimal("-10.75")}
    for revision, capture in enumerate(records, 1):
        signed = (fixture / f"statement-r{revision}.json").read_bytes()
        digest = hashlib.sha256(signed).hexdigest()
        receipt = capture["receipt"]
        if (receipt.get("signed_statement_sha256") != digest or receipt.get("statement_id") !=
                f"custody-batch.v1.synthetic-source.r{revision}.{digest}" or receipt.get("lookup") != "retained_run"
                or receipt.get("account_assignment_verified") is not True or not receipt.get("run_id")
                or receipt["run_id"] in runs or not re.fullmatch(r"[0-9a-f]{64}", capture["sha256"])):
            raise RuntimeError("Execution receipt does not identify its original authorized statement.")
        runs.add(receipt["run_id"])
        tx = json.loads(signed)["statement"]["transactions"][0]
        row, = receipt["execution_economics"]["rows"]
        if row["external_ref"] != tx["externalRef"] or row["instrument_id"] != tx["instrumentId"]:
            raise RuntimeError("Execution reference does not match the signed transaction.")
        if revision >= 3:
            reason = "source_cash_inconsistent" if revision == 3 else "convention_missing"
            if (row["status"] != "unknown" or row.get("reason") != reason
                    or any(k in row for k in ("book", "custodian", "difference"))):
                raise RuntimeError("Missing or inconsistent economics produced an assessed amount.")
            continue
        fee = amount(tx["executionEconomics"]["commission"])
        qty, price = amount(tx["quantity"]), amount(tx["price"])
        gross = -qty * price
        source = dict(quantity=qty, price=price, commission=fee, gross_cash=gross,
                      net_cash=gross - fee, settled_cash=gross - fee)
        if amount(tx["cash"]) != source["settled_cash"]:
            raise RuntimeError("Invalid synthetic settled cash oracle.")
        difference = {key: book[key] - value for key, value in source.items()}
        if (row["status"] != ("mismatch" if any(difference.values()) else "equal") or
                row["book_settlement_date"] != "2020-02-04" or row["custodian_settlement_date"] != "2020-02-04" or
                row["currency_code"] != "USD"):
            raise RuntimeError("Incorrect settlement status, date or units.")
        for group, expected in (("book", book), ("custodian", source), ("difference", difference)):
            if {key: amount(row[group][key]) for key in expected} != expected:
                raise RuntimeError("Execution economics differed from the independent decimal oracle.")
    return records


def verify_historical_comparison(path, fixture):
    if path.stat().st_size > 8192:
        raise RuntimeError("Historical comparison receipt exceeds its pilot bound.")
    record = json.loads(path.read_bytes())
    if not isinstance(record, dict) or record.keys() != {
        "portfolio_id", "custodian_id", "date", "original_run", "original_statement",
        "following_date", "following_run", "following_statement", "break_id", "statement_id", "run_id",
    } or not all(isinstance(value, str) and value for value in record.values()):
        raise RuntimeError("Historical comparison receipt is incomplete.")
    for name, revision, statement, day in (
        ("statement-r1", 1, "original_statement", "date"),
        ("following-r1", 1, "following_statement", "following_date"),
        ("statement-r2", 2, "statement_id", "date"),
    ):
        raw = (fixture / (name + ".json")).read_bytes()
        statement_id = f"custody-batch.v1.synthetic-source.r{revision}.{hashlib.sha256(raw).hexdigest()}"
        if record[statement] != statement_id or record[day] != json.loads(raw)["statement"]["businessDate"].split("T")[0]:
            raise RuntimeError("Historical comparison differs from its signed source.")
    if (record["portfolio_id"] != "PF1" or record["custodian_id"] != "CUST-A"
            or not re.fullmatch(r"q1:[0-9a-f]{64}", record["break_id"])
            or len({record["original_run"], record["following_run"], record["run_id"]}) != 3):
        raise RuntimeError("Historical comparison was not independently retained.")
    request = uuid.UUID(record["run_id"].removeprefix("manual:"))
    if record["run_id"] != "manual:" + str(request) or request.version != 4:
        raise RuntimeError("Historical comparison has no manual request identity.")
    return record


def run_pitr_source(command, env, directory, pitr, container):
    # Use a private output file so a full pipe cannot stall the child at the cut.
    log_path = directory / "source.log"
    with log_path.open("wb") as log:
        process = subprocess.Popen(command, cwd=CMD, env=env, stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 150
            while process.poll() is None and not (directory / "cut.json").is_file():
                if time.monotonic() >= deadline:
                    raise RuntimeError("The source pilot did not reach its restore point.")
                time.sleep(0.1)
            if process.poll() is None:
                pitr.mark(container)
                (directory / "cut-ready").touch(exist_ok=False)
            code = process.wait(timeout=max(1, deadline - time.monotonic()))
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=15)
    if log_path.stat().st_size > 256 * 1024:
        raise RuntimeError("Source verification log exceeds its bound.")
    return subprocess.CompletedProcess(command, code, stdout=log_path.read_bytes())


def qualify(docker, container, binaries, node, env, observations, profile, pitr=None):
    economics = profile == "execution_economics"
    booked_fee = profile == "booked_fee_correction"
    historical = profile == "historical_comparison"
    rerun = profile == "operator_comparisons" or historical
    quantity_evidence = profile == "quantity_comparisons"
    late = profile == "historical_quantity_correction"
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
    fixture = CMD / "testdata"
    if quantity_evidence:
        fixture = fixture / "instrument_units"
    elif late or historical:
        fixture = fixture / "late_quantity"
    elif economics or booked_fee:
        fixture = fixture / "execution_economics"
    authority = json.loads((fixture / "authority.json").read_bytes())
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
        GRANT SELECT, INSERT ON custody_execution_quantity_observations TO pilot_check;
        GRANT USAGE ON SEQUENCE outbox_id_seq, custody_execution_quantity_observations_observation_seq TO pilot_check;
    """)
    sql("postgres", (
        "CREATE ROLE pilot_source LOGIN NOSUPERUSER NOBYPASSRLS IN ROLE kanz_custody_batch_pilot "
        f"PASSWORD '{source_password}';"
    ).encode())

    if booked_fee:
        sql(DATABASE, b"""
            GRANT SELECT, INSERT ON execution_fee_revisions TO pilot_check;
            GRANT USAGE ON SEQUENCE execution_fee_revisions_revision_sequence_seq TO pilot_check;
        """)
    endpoint = subprocess.check_output([*docker, "port", container, "5432/tcp"], env=env, text=True).strip()
    if not re.fullmatch(r"127\.0\.0\.1:[1-9]\d{0,4}", endpoint) or int(endpoint.split(":")[1]) > 65535:
        raise RuntimeError("PostgreSQL must have exactly one literal loopback port.")
    reader_url = f"postgres://pilot_check:{owner_password}@{endpoint}/{DATABASE}?sslmode=disable"
    check_withdrawal = enroll_accounts(sql, DATABASE, endpoint, reader_url, binaries, env,
                                       CMD.parent / "custody_proof" / "testdata",
                                       check_startup=profile == "comparisons")
    pilot_env = dict(env,
        TEST_POSTGRES_URL=reader_url,
        TEST_CUSTODY_BATCH_DATABASE_URL=f"postgres://pilot_source:{source_password}@{endpoint}/{DATABASE}?sslmode=disable",
        CUSTODY_PILOT_VERIFY="1", CUSTODY_PILOT_GATEWAY_PROBE=str(binaries["gateway"]),
        CUSTODY_PILOT_BFF_PROBE=str(binaries["bff"]), TEST_MFA_NODE=node,
        TEST_MFA_STATIC_DIR=str(WEB / "dist"), TEST_MFA_PLAYWRIGHT=str(WEB / "node_modules/playwright"),
        CUSTODY_PILOT_BROWSER_SCRIPT=str(WEB / "e2e" / ("custody-rerun.cjs" if rerun else "custody-detection.cjs")),
        CUSTODY_PILOT_DATED_SCRIPT=str(WEB / "e2e/custody-dated.cjs"), CUSTODY_PILOT_RECEIPT=str(observations),
    )
    if pitr is not None:
        if not historical:
            raise RuntimeError("Physical recovery requires the historical comparison profile.")
        pitr.base_backup(container)
        pilot_env["CUSTODY_PILOT_PITR_DIR"] = str(observations.parent)
        pilot_env["CUSTODY_PILOT_PITR_BROWSER_SCRIPT"] = str(WEB / "e2e/custody-pitr.cjs")
        pilot_env["CUSTODY_PILOT_RETAINED_DOWNLOADS"] = str(observations.parent / "before-recovery.json")
    if profile == "comparisons":
        pilot_env["CUSTODY_PILOT_PREVIOUS_SCRIPT"] = str(WEB / "e2e/custody-previous-breaks.cjs")
    if quantity_evidence:
        pilot_env["CUSTODY_PILOT_QUANTITY_FIXTURE"] = "1"
    if late:
        pilot_env["CUSTODY_PILOT_LATE_QUANTITY"] = "1"
        pilot_env["CUSTODY_PILOT_BROWSER_SCRIPT"] = str(WEB / "e2e/custody-late-correction.cjs")
    if historical:
        pilot_env["CUSTODY_PILOT_BROWSER_SCRIPT"] = str(WEB / "e2e/custody-historical-comparison.cjs")
    if economics or booked_fee:
        pilot_env["CUSTODY_PILOT_IMPORTER"] = str(binaries["source"])
        pilot_env["CUSTODY_PILOT_SERVICE"] = str(binaries["service"])
        script = "custody-booked-fee.cjs" if booked_fee else "custody-execution-economics.cjs"
        pilot_env["CUSTODY_PILOT_BROWSER_SCRIPT"] = str(WEB / "e2e" / script)
    if late or quantity_evidence or economics or booked_fee:
        pilot_env["CUSTODY_PILOT_RETAINED_DOWNLOADS"] = str(observations.with_suffix(".downloads.json"))
    test = "TestSignedCustodyAuthorizedRerun" if rerun else "TestSignedCustodyOperatorReadback"
    if historical:
        test = "TestSignedHistoricalComparisonPilot"
    if economics:
        test = "TestSignedExecutionEconomicsPilot"
    if booked_fee:
        test = "TestSignedBookedFeePilot"
    command = [binaries["accounting"], "-test.run=^" + test + "$", "-test.v", "-test.count=1"]
    if pitr is None:
        proof = subprocess.run(command, cwd=CMD, env=pilot_env, capture_output=True, timeout=155)
    else:
        proof = run_pitr_source(command, pilot_env, observations.parent, pitr, container)
    output = proof.stdout
    markers = [b"signed customer recovered the applied historical quantity correction"] if late else [
        b"signed gateway and Chromium verified the persisted detection"]
    if economics:
        markers = [b"scheduled missing statement displayed and downloaded through Chromium"]
        markers.extend(f"signed economics revision {revision} retained and reopened through Chromium".encode()
                       for revision in range(1, 5))
    if historical:
        markers = [b"signed historical comparison preserved the later close and live queue through Chromium"]
    if booked_fee:
        markers = [b"signed Chromium verified unchanged-source booked fee correction and original receipt"]
    if not (late or rerun or economics or booked_fee):
        markers.append(b"signed customer verified clean, missing and unobserved business dates")
    if profile == "comparisons":
        markers.append(b"signed Chromium discovered the original receipt from a revised comparison with breaks")
    if (proof.returncode or b"--- SKIP" in output or ("--- PASS: " + test).encode() not in output
            or not all(marker in output for marker in markers)):
        stage = re.search(rb"custody-browser-stage=(input|launch|(?:authorized|foreign|excluded|unfunded)-(?:open|reload)-(?:response|payload|row|details))", output)
        if late:
            stage = re.search(rb"custody-late-browser-stage=(input|launch|(?:authorized|foreign|excluded|unfunded)-(?:open|tenant-portfolio-permission)|pending-corrected-source|correction-discovery|correction-recovery|original-and-following|retain-corrective-download)\b", output)
        if profile == "comparisons" and not stage:
            stage = re.search(rb"custody-previous-browser-stage=(input|launch|authorized|foreign|excluded|unfunded)\b", output)
        if booked_fee and not stage:
            stage = re.search(rb"custody-browser-stage=([a-z0-9-]+)", output)
        suffix = " at " + stage.group(1).decode() if stage else ""
        if not stage:
            locations = re.findall(rb"\b(operator_test|pilot_test|rollback_test|late_quantity_test|historical_comparison_pilot_test|execution_economics_pilot_test|booked_fee_pilot_test)\.go:([0-9]+):", output)
            if locations:
                file, line = locations[-1]
                suffix = " at " + file.decode() + ".go:" + line.decode()
        raise RuntimeError(test + " failed" + suffix + ".")
    if pitr is not None:
        source = verify_historical_comparison(observations, fixture)
        pitr.archive_after_target(container)
        # Only the readback process receives the saved synthetic reader URL.
        del pilot_env["TEST_CUSTODY_BATCH_DATABASE_URL"], pilot_env["CUSTODY_PILOT_PITR_DIR"], pilot_env["CUSTODY_PILOT_VERIFY"]
        return source, pilot_env
    if historical:
        return verify_historical_comparison(observations, fixture), None
    if economics:
        return verify_execution_economics(observations, fixture), None
    if booked_fee:
        return verify_booked_fee(observations, fixture), None
    records = verify_late_correction(observations, fixture) if late else verify_observations(observations, fixture, rerun, quantity_evidence)
    return records, check_withdrawal() if check_withdrawal else None


def verify_booked_fee(observations, fixture):
    digest = hashlib.sha256((fixture / "statement-r1.json").read_bytes()).hexdigest()
    expected_id = "custody-batch.v1.synthetic-source.r1." + digest
    results = {}
    for phase, outcome, position in (("original", "clean", "1"), ("corrected", "breaks", "2")):
        path = observations.with_suffix(".downloads.json.fee-" + phase)
        if not 0 < path.stat().st_size <= 64 << 10:
            raise RuntimeError("Booked fee receipt exceeds its pilot limit.")
        raw = path.read_bytes()
        row = json.loads(raw)
        if (row.get("signed_statement_sha256"), row.get("statement_id"), row.get("outcome"), row.get("journal_position")) != (
            digest, expected_id, outcome, position
        ):
            raise RuntimeError("Booked fee receipts do not identify the same signed source and distinct journal positions.")
        results[phase] = {"sha256": hashlib.sha256(raw).hexdigest(), "receipt": row}
    if results["original"]["receipt"]["run_id"] == results["corrected"]["receipt"]["run_id"]:
        raise RuntimeError("Booked fee correction reused the original run identity.")
    return results


def customer_downloads(observations, records):
    path = observations.with_suffix(".downloads.json")
    if path.stat().st_size > 128 * 1024:
        raise RuntimeError("The synthetic browser receipt exceeds its export limit.")
    receipts = json.loads(path.read_bytes())
    if not isinstance(receipts, list) or len(receipts) != len(records):
        raise RuntimeError("Expected a customer download for both signed revisions.")
    for receipt, record in zip(receipts, records):
        if (not isinstance(receipt, dict) or receipt.get("format") != "kanz.custody-comparison.v1"
                or receipt.get("lookup") != "retained_run"
                or not isinstance(receipt.get("breaks"), list) or len(receipt["breaks"]) != record["retained_breaks"]
                or receipt.get("account_assignment_verified") is not True
                or receipt.get("run_id") != record["run_id"] or receipt.get("statement_id") != record["statement_id"]
                or receipt.get("signed_statement_sha256") != record["statement_id"].rsplit(".", 1)[-1]):
            raise RuntimeError("Browser download does not match the independently stored comparison.")
    return receipts


def run_profile(docker, binaries, node, env, observations, profile, pitr=None):
    # Local SQL administration uses peer auth. The disposable remote roles use SCRAM.
    name = "kanz-custody-pilot-" + secrets.token_hex(8)
    command = (
        "umask 077; head -c 32 /dev/urandom | base64 > /run/pilot/admin; "
        "chown postgres /run/pilot/admin; exec docker-entrypoint.sh postgres"
        if pitr is None else pitr.primary_command()
    )
    recovery_args = [] if pitr is None else pitr.primary_args()
    args = [*docker, "run", "--detach", "--rm", "--pull=never", "--name", name,
            "--publish", "127.0.0.1::5432", "--tmpfs", "/var/lib/postgresql/data",
            "--tmpfs", "/run/pilot", "--env", "POSTGRES_INITDB_ARGS=--auth-local=peer --auth-host=scram-sha-256",
            "--env", "POSTGRES_PASSWORD_FILE=/run/pilot/admin", *recovery_args,
            "--entrypoint", "sh", IMAGE, "-ec", command]
    container = subprocess.check_output(args, env=env, text=True, timeout=30).strip()
    try:
        return qualify(docker, container, binaries, node, env, observations, profile, pitr)
    finally:
        subprocess.run([*docker, "rm", "--force", container], env=env, check=True,
                       stdout=subprocess.DEVNULL, timeout=30)


def verify_physical_recovery(docker, binaries, node, env, directory):
    with CustodyPITR(docker, env, IMAGE) as pitr:
        source, read_env = run_profile(docker, binaries, node, env, directory / "source.json",
                                       "historical_comparison", pitr)
        cut_file = directory / "cut.json"
        if cut_file.stat().st_size > 8192:
            raise RuntimeError("Recovery target metadata exceeds its bound.")
        cut = json.loads(cut_file.read_bytes())
        expected = {key: source[key] for key in (
            "portfolio_id", "custodian_id", "date", "original_run", "original_statement",
            "following_date", "following_run", "following_statement",
        )}
        if cut != dict(expected, phase="source"):
            raise RuntimeError("Recovery target does not identify the observed source close.")
        metadata = json.dumps(dict(expected, phase="restored", future_run=source["run_id"],
                                   future_statement=source["statement_id"])).encode("utf-8")
        # The primary has been removed. Only the restored cluster can answer.
        _, endpoint = pitr.restore()
        if not re.fullmatch(r"127\.0\.0\.1:[1-9]\d{0,4}", endpoint) or int(endpoint.split(":")[1]) > 65535:
            raise RuntimeError("The restored reader must have a literal loopback address.")
        credential, sep, old_endpoint = read_env["TEST_POSTGRES_URL"].partition("@")
        if sep != "@" or not credential.startswith("postgres://pilot_check:") or not old_endpoint.startswith("127.0.0.1:"):
            raise RuntimeError("The source reader is outside the isolated pilot.")
        after_file = directory / "after-recovery.json"
        read_env.update(
            TEST_POSTGRES_URL=f"{credential}@{endpoint}/{DATABASE}?sslmode=disable",
            CUSTODY_PILOT_PITR_READBACK="1",
            CUSTODY_PILOT_RETAINED_DOWNLOADS=str(after_file),
        )
        proof = subprocess.run(
            [binaries["accounting"], "-test.run=^TestSignedCustodyPITRReadback$", "-test.v", "-test.count=1"],
            cwd=CMD, env=read_env, input=metadata, capture_output=True, timeout=75,
        )
        if (proof.returncode or b"--- SKIP" in proof.stdout
                or b"--- PASS: TestSignedCustodyPITRReadback" not in proof.stdout):
            stage = re.search(rb"custody-browser-stage=[a-z-]{1,50}|custody_pitr_readback_test\.go:[0-9]+", proof.stdout)
            where = " at " + stage.group().decode() if stage else ""
            raise RuntimeError("Restored custody browser readback failed" + where + ".")
        pitr.verify_paused()
        before_file = directory / "before-recovery.json"
        if not 1 <= before_file.stat().st_size <= 1024 * 1024 or not 1 <= after_file.stat().st_size <= 1024 * 1024:
            raise RuntimeError("Recovery receipt exceeds its pilot bound.")
        before, after = before_file.read_bytes(), after_file.read_bytes()
        if before != after:
            raise RuntimeError("The restored dated browser receipt differs from the source.")
        return {"postgres_image": IMAGE, "source": source, "recovery": pitr.evidence,
                "identical_browser_receipt_sha256": hashlib.sha256(after).hexdigest()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node", default="node", help="Node executable matching web/.nvmrc")
    parser.add_argument("--pull-image", action="store_true", help="Fetch the pinned PostgreSQL image before startup")
    parser.add_argument("--pitr", action="store_true", help="Run the isolated physical recovery drill")
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
        if args.pitr:
            results = verify_physical_recovery(docker, binaries, node, env, Path(directory))
        else:
            results = {"postgres_image": IMAGE}
            for key in ("comparisons", "operator_comparisons", "quantity_comparisons", "historical_quantity_correction",
                        "execution_economics", "historical_comparison", "booked_fee_correction"):
                observations = Path(directory) / (key + ".json")
                results[key], onboarding = run_profile(docker, binaries, node, env, observations, key)
                if onboarding:
                    results["account_onboarding"] = onboarding
                if key == "execution_economics":
                    missing_path = observations.with_suffix(".downloads.json.missing")
                    if missing_path.stat().st_size > 8192:
                        raise RuntimeError("Missing statement receipt exceeds its pilot bound.")
                    results["missing_statement_recovery"] = json.loads(missing_path.read_bytes())
                if key == "quantity_comparisons":
                    results["customer_downloads"] = customer_downloads(observations, results[key])
    if args.receipt:
        args.receipt.write_text(json.dumps(results, indent=2) + "\n",
                                encoding="utf-8")
    if args.pitr:
        print("PASS: Physical WAL recovery stopped at the named target. The signed customer "
              "downloaded the original receipt byte for byte, while the later correction was absent.")
        return
    print("PASS: The signed customer read the discrepancy, corrected clean date, missing statement and "
          "unobserved date in Chromium. The customer could not post cash, change the break or start a rerun. "
          "A separate operator replayed one request with one durable receipt and completion FACT. "
          "After a later close, the customer downloaded the quantity correction against the original execution. "
          "Four signed execution revisions showed a mismatch, agreement, inconsistent cash and a missing convention. "
          "The original execution receipt reopened unchanged after correction. The first late statement replaced the scheduled missing result, and both receipts reopened unchanged. "
          "A full historical comparison reopened in Chromium and preserved the later close and live queue. "
          "A corrected comparison with a new break still opened its original signed receipt. "
          "A booked fee correction retained both outcomes for one signed source.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
