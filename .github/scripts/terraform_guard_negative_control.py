"""Qualify the reviewed mock-only Terraform gate without changing the checkout.

Terraform 1.15.8's JSON test UI is the evidence interface. A nonzero CLI exit
alone is never evidence that the AMI approval assertion actually ran.
"""

import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile


VERSION = "1.15.8"
FIXTURE = "testnet.tftest.hcl"
INPUTS = (
    "main.tf", "variables.tf", "versions.tf", "outputs.tf",
    "user_data.sh.tftpl", FIXTURE, ".terraform.lock.hcl",
)
RUNS = (
    "cost_and_security_envelope", "rejects_non_tokyo_region",
    "rejects_non_osaka_recovery_region", "rejects_cost_expansion",
)
ASSERTION = "The Tokyo node AMI changed without a reviewed rotation and rollback plan."
AMI = b'ami-0794a632d5c1058bf'
MUTANT_AMI = b'ami-00000000000000000'
MAX_OUTPUT = 8 * 1024 * 1024


class QualificationError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise QualificationError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def blocks(text):
    """Read top-level blocks in this fixed fixture profile, ignoring comments.

    This is a bounded preflight, not a replacement HCL validator. Terraform
    validates the complete copied configuration before any mock test runs.
    """
    tokens = re.findall(r'//[^\n]*|\#[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\])*"|[\w-]+|[^\s]', text, re.S)
    tokens = [t for t in tokens if not t.startswith(("//", "#", "/*"))]
    result = []
    index = 0
    while index < len(tokens):
        kind = tokens[index]
        index += 1
        labels = []
        while index < len(tokens) and tokens[index].startswith('"'):
            labels.append(json.loads(tokens[index]))
            index += 1
        require(index < len(tokens) and tokens[index] == "{", "unsupported top-level HCL input")
        index += 1
        start, depth = index, 1
        while index < len(tokens) and depth:
            if tokens[index] == "{":
                depth += 1
            elif tokens[index] == "}":
                depth -= 1
            index += 1
        require(depth == 0, "unterminated HCL block")
        result.append((kind, labels, tokens[start:index - 1]))
    return result


def read_inputs(module):
    require(not module.is_symlink() and module.is_dir(), "module must be a regular directory")
    actual = {
        p.name for p in module.iterdir()
        if p.name.endswith((".tf", ".tf.json", ".tftest.hcl", ".tftest.json"))
    }
    require(actual == {n for n in INPUTS if n.endswith((".tf", ".tftest.hcl"))},
            "unexpected Terraform input; review the seven-file mock profile")
    data = {}
    for name in INPUTS:
        path = module / name
        require(stat.S_ISREG(path.lstat().st_mode), "input must be a regular file: " + name)
        require(path.stat().st_size <= MAX_OUTPUT, "input is too large: " + name)
        data[name] = path.read_bytes()

    fixture_blocks = blocks(data[FIXTURE].decode("utf-8"))
    mocks = [(labels, body) for kind, labels, body in fixture_blocks if kind == "mock_provider"]
    require(len(mocks) == 2 and all(labels == ["aws"] for labels, _ in mocks),
            "both AWS mock configurations are required")
    aliases = []
    for _, body in mocks:
        positions = [i for i, token in enumerate(body) if token == "alias"]
        require(len(positions) <= 1, "ambiguous mock alias")
        if positions:
            require(body == ["alias", "=", '"recovery"'], "unreviewed mock alias")
        aliases.append(bool(positions))
    require(sorted(aliases) == [False, True], "default and recovery mocks are required")
    require([(kind, labels) for kind, labels, _ in fixture_blocks if kind != "mock_provider"]
            == [("run", [name]) for name in RUNS], "fixture run inventory changed")
    require(not any(token in {"providers", "module", "provider"}
                    for kind, _, body in fixture_blocks for token in body),
            "fixture must use only the reviewed mocks and local module")
    providers = [(labels, body) for kind, labels, body
                 in blocks(data["versions.tf"].decode("utf-8")) if kind == "provider"]
    require(len(providers) == 2 and all(labels == ["aws"] for labels, _ in providers),
            "unreviewed provider configuration")
    provider_aliases = []
    for _, body in providers:
        depth, aliases = 0, []
        for i, token in enumerate(body):
            if depth == 0 and token == "alias":
                require(body[i:i + 3] == ["alias", "=", '"recovery"'], "unreviewed provider alias")
                aliases.append("recovery")
            depth += int(token in {"{", "[", "("}) - int(token in {"}", "]", ")"})
        require(len(aliases) <= 1, "ambiguous provider alias")
        provider_aliases.append(aliases)
    require(sorted(provider_aliases) == [[], ["recovery"]], "recovery provider configuration changed")

    ami_pattern = rb'(?m)^\s*ami\s*=\s*"' + AMI + rb'"\s*$'
    require(len(re.findall(ami_pattern, data["main.tf"])) == 1
            and data["main.tf"].count(AMI) == 1, "AMI mutation must identify exactly one assignment")
    condition = 'condition     = aws_instance.node.ami == "' + AMI.decode() + '"'
    lines = data[FIXTURE].decode("utf-8").splitlines()
    locations = [i + 1 for i, line in enumerate(lines) if line.strip() == condition]
    require(len(locations) == 1 and data[FIXTURE].decode().count(ASSERTION) == 1,
            "named AMI assertion changed")
    return data, locations[0]


def child_environment(scratch):
    # Start empty: neither AWS/container credentials nor TF_CLI_ARGS/TF_VAR_*
    # and host CLI credential helpers may enter the disposable test process.
    env = {name: os.environ[name] for name in ("PATH", "SystemRoot") if name in os.environ}
    home = scratch / "home"
    home.mkdir()
    config = scratch / "terraform.rc"
    config.write_text("", encoding="utf-8")
    env.update({
        "HOME": str(home), "USERPROFILE": str(home),
        "TMPDIR": str(scratch), "TMP": str(scratch), "TEMP": str(scratch),
        "TF_DATA_DIR": str(scratch / "terraform-data"),
        "TF_CLI_CONFIG_FILE": str(config),
        "TF_IN_AUTOMATION": "true", "TF_INPUT": "0", "CHECKPOINT_DISABLE": "1",
        "AWS_EC2_METADATA_DISABLED": "true",
        "AWS_SHARED_CREDENTIALS_FILE": os.devnull, "AWS_CONFIG_FILE": os.devnull,
    })
    return env


def run_cli(argv, cwd, env):
    result = subprocess.run(argv, cwd=cwd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=600, check=False)
    require(len(result.stdout) <= MAX_OUTPUT, "Terraform output exceeded the evidence bound")
    return result.returncode, result.stdout.decode("utf-8", errors="strict")


def parse_phase(output, exit_code, assertion_line, negative=False):
    require(len(output.encode("utf-8")) <= MAX_OUTPUT and output.endswith("\n"),
            "empty, truncated or oversized JSON test output")
    try:
        records = [json.loads(line) for line in output.splitlines()]
    except (ValueError, TypeError) as error:
        raise QualificationError("invalid Terraform JSON test output") from error
    require(records and all(isinstance(r, dict) and isinstance(r.get("type"), str) for r in records),
            "invalid JSON UI record")
    versions = [r for r in records if r["type"] == "version"]
    require(len(versions) == 1 and records[0] == versions[0]
            and versions[0].get("terraform") == VERSION
            and re.fullmatch(r"1\.\d+", versions[0].get("ui", "")), "wrong Terraform/UI version")
    abstracts = [r.get("test_abstract") for r in records if r["type"] == "test_abstract"]
    require(abstracts == [{FIXTURE: list(RUNS)}], "missing or changed declared run inventory")
    require(not any(r["type"].startswith(("test_cleanup", "test_interrupt", "test_fatal_interrupt"))
                    or "state" in r for r in records), "cleanup/interrupt/state output cannot qualify")
    terminals, errors, files = {}, [], []
    for r in records:
        # Envelope errors apply to every record type. Only the negative phase's
        # single error diagnostic may proceed to the exact assertion checks below.
        if r.get("@level") == "error":
            require(negative and r["type"] == "diagnostic"
                    and isinstance(r.get("diagnostic"), dict)
                    and r["diagnostic"].get("severity") == "error",
                    "unexpected Terraform error envelope")
        if r["type"] == "test_run":
            run = r.get("test_run", {})
            require(isinstance(run, dict) and run.get("path") == FIXTURE
                    and run.get("run") in RUNS and r.get("@testfile") == FIXTURE
                    and r.get("@testrun") == run.get("run"), "unexpected test run identity")
            if run.get("progress") == "complete":
                name, status = run["run"], run.get("status")
                require(name not in terminals and status in {"pass", "fail", "error", "skip"},
                        "duplicate or invalid terminal run outcome")
                terminals[name] = status
        elif r["type"] == "test_file":
            file = r.get("test_file", {})
            require(isinstance(file, dict) and file.get("path") == FIXTURE
                    and r.get("@testfile") == FIXTURE, "unexpected test file identity")
            if file.get("progress") == "complete":
                files.append(file.get("status"))
        elif r["type"] == "diagnostic":
            diagnostic = r.get("diagnostic", {})
            require(isinstance(diagnostic, dict) and diagnostic.get("severity") in {"error", "warning"},
                    "invalid diagnostic")
            if diagnostic["severity"] == "error":
                errors.append(r)
    require(set(terminals) == set(RUNS), "incomplete terminal run inventory")
    counts = Counter(terminals.values())
    summaries = [r for r in records if r["type"] == "test_summary"]
    expected_status = "fail" if negative else "pass"
    require(files == [expected_status], "missing, duplicate or inconsistent terminal file outcome")
    expected_summary = {"status": expected_status, "passed": counts["pass"],
                        "failed": counts["fail"], "errored": counts["error"], "skipped": counts["skip"]}
    require(len(summaries) == 1 and summaries[0].get("test_summary") == expected_summary,
            "missing, duplicate or inconsistent test summary")
    if negative:
        require(exit_code == 1 and terminals[RUNS[0]] == "fail" and counts["fail"] == 1
                and counts["error"] == 0 and len(errors) == 1,
                "negative phase did not fail only the target assertion")
        record, = errors
        diagnostic = record["diagnostic"]
        location = diagnostic.get("range", {})
        require(record.get("@testfile") == FIXTURE and record.get("@testrun") == RUNS[0]
                and diagnostic.get("summary") == "Test assertion failed"
                and diagnostic.get("detail") == ASSERTION and location.get("filename") == FIXTURE
                and location.get("start", {}).get("line") == assertion_line,
                "negative phase failed for an unrelated reason")
    else:
        require(exit_code == 0 and counts["pass"] == len(RUNS) and not errors,
                "positive phase requires every run to pass")
    return {"runs": terminals, "summary": expected_summary, "errors": errors,
            "output_sha256": digest(output.encode("utf-8")), "exit_code": exit_code}


def qualify(module, scratch_parent, terraform, runner=run_cli, emit=print):
    original, assertion_line = read_inputs(module)
    hashes = {name: digest(data) for name, data in original.items()}
    receipt = {"terraform": VERSION, "input_sha256": hashes, "assertion_line": assertion_line, "phases": {}}
    with tempfile.TemporaryDirectory(prefix="terraform-guard-", dir=scratch_parent) as directory:
        scratch = Path(directory)
        work = scratch / "module"
        work.mkdir()
        for name, data in original.items():
            (work / name).write_bytes(data)
        env = child_environment(scratch)

        def command(*args):
            code, output = runner([terraform, *args], work, env)
            require(len(output.encode("utf-8")) <= MAX_OUTPUT, "oversized command output")
            return code, output

        code, version_output = command("version", "-json")
        require(code == 0 and json.loads(version_output).get("terraform_version") == VERSION,
                "the pinned Terraform runtime is required")
        for args in (("fmt", "-check", "-recursive"),
                     ("init", "-backend=false", "-input=false", "-lockfile=readonly", "-no-color"),
                     ("validate", "-no-color")):
            code, output = command(*args)
            emit(json.dumps({"terraform_setup": args[0], "exit_code": code,
                             "output_sha256": digest(output.encode())}))
            require(code == 0, args[0] + " failed; no negative-control credit")

        def phase(name, negative=False):
            expected = dict(hashes)
            if negative:
                expected["main.tf"] = receipt["mutant_main_sha256"]
            require({n: digest((work / n).read_bytes()) for n in INPUTS} == expected,
                    "phase inputs differ from the reviewed copy")
            code, output = command("test", "-json", "-no-color", "-filter=" + FIXTURE)
            emit(json.dumps({"terraform_phase": name, "output_sha256": digest(output.encode())}))
            emit(output.rstrip("\n"))
            receipt["phases"][name] = parse_phase(output, code, assertion_line, negative)
            require({n: digest((work / n).read_bytes()) for n in INPUTS} == expected,
                    "test execution modified an input")
            receipt["phases"][name]["input_sha256"] = expected

        phase("positive")
        try:
            mutated = original["main.tf"].replace(AMI, MUTANT_AMI)
            (work / "main.tf").write_bytes(mutated)
            receipt["mutant_main_sha256"] = digest(mutated)
            phase("negative", negative=True)
        finally:
            # Also restore and run the positive control when negative evidence is
            # invalid. An unrelated failure must not masquerade as qualification.
            (work / "main.tf").write_bytes(original["main.tf"])
            require({name: digest((work / name).read_bytes()) for name in INPUTS} == hashes,
                    "copied fixture was not restored byte-for-byte")
            phase("restored")
            require({name: digest((work / name).read_bytes()) for name in INPUTS} == hashes,
                    "restored test modified an input")
    require({name: digest((module / name).read_bytes()) for name in INPUTS} == hashes,
            "source checkout changed during qualification")
    return receipt


def main():
    root = Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--module", type=Path, default=root / "kanz/infra/terraform-testnet")
    args = parser.parse_args()
    terraform = shutil.which("terraform")
    require(terraform is not None, "verified Terraform binary is unavailable")
    receipt = qualify(args.module, os.environ.get("RUNNER_TEMP"), terraform)
    receipt["source"] = {
        "commit": subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip(),
        "tree": subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD^{tree}"], text=True).strip(),
        **{name: os.environ.get(name, "") for name in
           ("GITHUB_SHA", "PR_HEAD_SHA", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_EVENT_NAME")},
    }
    print(json.dumps({"terraform_guard_qualification": receipt}, sort_keys=True))


if __name__ == "__main__":
    main()
