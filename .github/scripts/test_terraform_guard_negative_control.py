"""Evidence-parser and disposable-copy controls; these do not run Terraform."""

import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import terraform_guard_negative_control as guard


ROOT = Path(__file__).resolve().parents[2]
MODULE = ROOT / "kanz/infra/terraform-testnet"


def transcript(negative=False):
    # Hand-built JSON UI records, using the published 1.15.8 test UI contract.
    statuses = ["fail", "skip", "skip", "skip"] if negative else ["pass"] * 4
    records = [
        {"type": "version", "terraform": "1.15.8", "ui": "1.3"},
        {"type": "test_abstract", "test_abstract": {
            "testnet.tftest.hcl": ["cost_and_security_envelope", "rejects_non_tokyo_region",
                                   "rejects_non_osaka_recovery_region", "rejects_cost_expansion"]}},
    ]
    for name, status in zip(guard.RUNS, statuses):
        records.append({"type": "test_run", "@testfile": "testnet.tftest.hcl", "@testrun": name,
                        "test_run": {"path": "testnet.tftest.hcl", "run": name,
                                     "progress": "complete", "status": status}})
    if negative:
        records.append({"type": "diagnostic", "@level": "error",
                        "@testfile": "testnet.tftest.hcl", "@testrun": "cost_and_security_envelope",
                        "diagnostic": {"severity": "error", "summary": "Test assertion failed",
                                       "detail": "The Tokyo node AMI changed without a reviewed rotation and rollback plan.",
                                       "range": {"filename": "testnet.tftest.hcl", "start": {"line": 30}}}})
    records.append({"type": "test_file", "@testfile": "testnet.tftest.hcl", "test_file": {
        "path": "testnet.tftest.hcl", "progress": "complete", "status": "fail" if negative else "pass"}})
    records.append({"type": "test_summary", "test_summary": {
        "status": "fail" if negative else "pass", "passed": 0 if negative else 4,
        "failed": 1 if negative else 0, "errored": 0, "skipped": 3 if negative else 0}})
    return records


def encode(records):
    return "".join(json.dumps(r) + "\n" for r in records)


class EvidenceTests(unittest.TestCase):
    def assert_rejected(self, records, code=1, negative=True):
        with self.assertRaises(guard.QualificationError):
            guard.parse_phase(encode(records), code, 30, negative)

    def test_positive_named_negative_and_restored_inventory(self):
        for negative in (False, True):
            with self.subTest(negative=negative):
                receipt = guard.parse_phase(encode(transcript(negative)), int(negative), 30, negative)
                self.assertEqual(len(receipt["runs"]), 4)
                self.assertEqual(receipt["summary"]["errored"], 0)

    def test_negative_other_runs_may_pass_but_never_error(self):
        records = transcript(True)
        records[3]["test_run"]["status"] = "pass"
        records[-1]["test_summary"].update(passed=1, skipped=2)
        guard.parse_phase(encode(records), 1, 30, True)
        records[3]["test_run"]["status"] = "error"
        records[-1]["test_summary"].update(passed=0, errored=1)
        self.assert_rejected(records)

    def test_generic_failure_or_phrase_in_log_never_qualifies(self):
        records = transcript(True)
        records[-3] = {"type": "log", "@message": guard.ASSERTION}
        self.assert_rejected(records)
        records = transcript(True)
        records[-3]["diagnostic"].update(summary="Failed to install provider", detail="Network unavailable")
        self.assert_rejected(records)
        for code in (0, 2, -1):
            with self.subTest(code=code):
                self.assert_rejected(transcript(True), code)

    def test_exact_assertion_file_run_and_source_location_required(self):
        mutations = (
            lambda r: r[-3].update({"@testfile": "other.tftest.hcl"}),
            lambda r: r[-3].update({"@testrun": "rejects_cost_expansion"}),
            lambda r: r[-3]["diagnostic"].update(detail=guard.ASSERTION + " extra"),
            lambda r: r[-3]["diagnostic"]["range"].update(filename="main.tf"),
            lambda r: r[-3]["diagnostic"]["range"]["start"].update(line=31),
            lambda r: r[-3]["diagnostic"].update(summary="Check block assertion failed"),
        )
        for index, mutate in enumerate(mutations):
            with self.subTest(index=index):
                records = transcript(True)
                mutate(records)
                self.assert_rejected(records)

    def test_additional_error_or_cleanup_or_interrupt_refuses(self):
        additions = (
            {"type": "diagnostic", "diagnostic": {"severity": "error", "summary": "Other failure"}},
            {"type": "test_cleanup", "test_cleanup": {"failed_resources": ["aws_instance.node"]}},
            {"type": "test_interrupt"}, {"type": "test_fatal_interrupt"},
            {"type": "log", "@level": "error", "@message": "provider crash"},
            {"type": "log", "state": {}},
        )
        for addition in additions:
            with self.subTest(addition=addition):
                self.assert_rejected(transcript(True) + [addition])

    def test_error_envelope_cannot_hide_in_recognized_record_types(self):
        additions = (
            {"type": "test_run", "@testfile": "testnet.tftest.hcl",
             "@testrun": "cost_and_security_envelope", "test_run": {
                 "path": "testnet.tftest.hcl", "run": "cost_and_security_envelope", "progress": "running"}},
            {"type": "test_file", "@testfile": "testnet.tftest.hcl", "test_file": {
                "path": "testnet.tftest.hcl", "progress": "running"}},
            {"type": "diagnostic", "diagnostic": {"severity": "warning", "summary": "Fixture warning"}},
        )
        for addition in additions:
            for negative in (False, True):
                with self.subTest(record_type=addition["type"], negative=negative):
                    record = {**addition, "@level": "error"}
                    self.assert_rejected(transcript(negative) + [record], code=int(negative), negative=negative)
        warning = {**additions[-1], "@level": "warn"}
        guard.parse_phase(encode(transcript() + [warning]), 0, 30)

    def test_incomplete_duplicate_unexpected_outcomes_and_summary_refuse(self):
        mutations = (
            lambda r: r.pop(3),
            lambda r: r.append(copy.deepcopy(r[2])),
            lambda r: r[2]["test_run"].update(run="undeclared"),
            lambda r: r[1]["test_abstract"][guard.FIXTURE].pop(),
            lambda r: r.append(copy.deepcopy(r[1])),
            lambda r: r.pop(),
            lambda r: r.append(copy.deepcopy(r[-1])),
            lambda r: r[-1]["test_summary"].update(passed=1),
            lambda r: r[2]["test_run"].update(progress="running"),
            lambda r: r.pop(-2),
            lambda r: r.append(copy.deepcopy(r[-2])),
        )
        for index, mutate in enumerate(mutations):
            with self.subTest(index=index):
                records = transcript(True)
                mutate(records)
                self.assert_rejected(records)

    def test_truncated_non_json_and_wrong_version_refuse(self):
        for output in ("", encode(transcript(True)).rstrip(), "network failure\n", "{}\n", "[1]\n",
                       encode(transcript(True)) + '{"type":'):
            with self.subTest(output=output[-30:]), self.assertRaises(guard.QualificationError):
                guard.parse_phase(output, 1, 30, True)
        for key, value in (("terraform", "1.15.7"), ("ui", "2.0")):
            records = transcript(True)
            records[0][key] = value
            self.assert_rejected(records)

    def test_positive_requires_every_run_pass_including_restoration(self):
        self.assert_rejected(transcript(True), code=0, negative=False)
        for status in ("skip", "fail", "error"):
            records = transcript()
            records[2]["test_run"]["status"] = status
            records[-1]["test_summary"].update(passed=3)
            records[-1]["test_summary"][{"skip": "skipped", "fail": "failed", "error": "errored"}[status]] = 1
            self.assert_rejected(records, code=0, negative=False)


class DisposableCopyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.module = self.root / "source"
        self.module.mkdir()
        self.inputs, self.line = guard.read_inputs(MODULE)
        for name, data in self.inputs.items():
            (self.module / name).write_bytes(data)
        self.test_calls = []

    def fake_runner(self, argv, cwd, env):
        self.assertEqual(argv[0], "verified-terraform")
        self.assertNotEqual(cwd, self.module)
        self.assertEqual(set(p.name for p in cwd.iterdir()), set(guard.INPUTS))
        self.assertTrue(Path(env["HOME"]).is_relative_to(cwd.parent))
        self.assertTrue(Path(env["TF_DATA_DIR"]).is_relative_to(cwd.parent))
        self.assertEqual(Path(env["TF_CLI_CONFIG_FILE"]).read_bytes(), b"")
        self.assertEqual(env["AWS_CONFIG_FILE"], os.devnull)
        self.assertEqual(env["AWS_SHARED_CREDENTIALS_FILE"], os.devnull)
        self.assertEqual(env["AWS_EC2_METADATA_DISABLED"], "true")
        self.assertFalse(any(k.startswith(("TF_VAR_", "TF_CLI_ARGS", "TF_TOKEN_", "ACTIONS_", "GITHUB_"))
                             for k in env))
        self.assertNotIn("AWS_ACCESS_KEY_ID", env)
        self.assertNotIn("AWS_WEB_IDENTITY_TOKEN_FILE", env)
        command = argv[1:]
        if command == ["version", "-json"]:
            return 0, '{"terraform_version":"1.15.8"}'
        if command == ["test", "-json", "-no-color", "-filter=testnet.tftest.hcl"]:
            data = {name: (cwd / name).read_bytes() for name in guard.INPUTS}
            negative = guard.MUTANT_AMI in data["main.tf"]
            self.assertEqual(data[guard.FIXTURE], self.inputs[guard.FIXTURE])
            for name in set(guard.INPUTS) - {"main.tf"}:
                self.assertEqual(data[name], self.inputs[name])
            self.assertEqual(data["main.tf"], self.inputs["main.tf"].replace(guard.AMI, guard.MUTANT_AMI)
                             if negative else self.inputs["main.tf"])
            self.test_calls.append((negative, cwd))
            records = transcript(negative)
            if negative:
                records[-3]["diagnostic"]["range"]["start"]["line"] = self.line
            return int(negative), encode(records)
        self.assertIn(command, [
            ["fmt", "-check", "-recursive"],
            ["init", "-backend=false", "-input=false", "-lockfile=readonly", "-no-color"],
            ["validate", "-no-color"],
        ])
        return 0, "fixture command completed\n"

    def qualify(self, runner=None):
        return guard.qualify(self.module, self.root, "verified-terraform", runner or self.fake_runner,
                             emit=lambda _: None)

    def test_original_inputs_and_safe_environment_across_three_phases(self):
        # Non-inputs are deliberately never copied, including the live wrapper.
        (self.module / ".terraform").mkdir()
        (self.module / ".terraform" / "secret").write_text("dummy")
        for name in ("terraform.tfstate", "saved.plan", "local.tfvars", "Invoke-Terraform.ps1"):
            (self.module / name).write_text("must not be copied")
        with patch.dict(os.environ, {"AWS_ACCESS_KEY_ID": "dummy", "AWS_WEB_IDENTITY_TOKEN_FILE": "dummy",
                                     "TF_CLI_ARGS": "apply", "TF_VAR_password": "dummy", "GITHUB_TOKEN": "dummy"}):
            receipt = self.qualify()
        self.assertEqual([negative for negative, _ in self.test_calls], [False, True, False])
        self.assertEqual(set(receipt["phases"]), {"positive", "negative", "restored"})
        self.assertTrue(all(not cwd.exists() for _, cwd in self.test_calls))
        self.assertEqual({n: (self.module / n).read_bytes() for n in guard.INPUTS}, self.inputs)

    def test_unrelated_negative_error_and_exception_still_restore_but_refuse(self):
        for exception in (False, True):
            with self.subTest(exception=exception):
                self.test_calls.clear()

                def runner(argv, cwd, env):
                    result = self.fake_runner(argv, cwd, env)
                    if len(self.test_calls) == 2 and argv[1] == "test":
                        if exception:
                            raise RuntimeError("fixture process error")
                        return 1, "not JSON\n"
                    return result

                with self.assertRaises((guard.QualificationError, RuntimeError)):
                    self.qualify(runner)
                self.assertEqual([negative for negative, _ in self.test_calls], [False, True, False])
                self.assertEqual((self.module / "main.tf").read_bytes(), self.inputs["main.tf"])

    def test_restored_failure_never_becomes_success(self):
        def runner(argv, cwd, env):
            result = self.fake_runner(argv, cwd, env)
            if len(self.test_calls) == 3 and argv[1] == "test":
                return 1, "fixture restored phase failed\n"
            return result

        with self.assertRaises(guard.QualificationError):
            self.qualify(runner)

    def test_setup_failure_cannot_receive_negative_control_credit(self):
        for command in ("version", "fmt", "init", "validate"):
            with self.subTest(command=command):
                def runner(argv, cwd, env):
                    return (1, "fixture network/CLI failure") if argv[1] == command else self.fake_runner(argv, cwd, env)

                with self.assertRaises(guard.QualificationError):
                    self.qualify(runner)
                self.assertEqual(self.test_calls, [])

    def test_unpinned_runtime_and_modified_fixture_refuse_before_test(self):
        def wrong_version(argv, cwd, env):
            if argv[1] == "version":
                return 0, '{"terraform_version":"1.15.7"}'
            return self.fake_runner(argv, cwd, env)

        def modified_fixture(argv, cwd, env):
            result = self.fake_runner(argv, cwd, env)
            if argv[1] == "init":
                (cwd / guard.FIXTURE).write_bytes(b'run "different" {}')
            return result

        for runner in (wrong_version, modified_fixture):
            with self.subTest(runner=runner.__name__), self.assertRaises(guard.QualificationError):
                self.qualify(runner)
            self.assertEqual(self.test_calls, [])

    def test_changed_fixture_profile_or_missing_input_refuses_before_execution(self):
        cases = (
            ("extra.tf", b'resource "aws_instance" "extra" {}'),
            ("testnet.tftest.hcl", self.inputs[guard.FIXTURE].replace(b'alias = "recovery"', b'# alias = "recovery"')),
            ("testnet.tftest.hcl", self.inputs[guard.FIXTURE].replace(b'mock_provider "aws"', b'provider "aws"', 1)),
            ("testnet.tftest.hcl", self.inputs[guard.FIXTURE].replace(
                b'alias = "recovery"', b'mock_data "aws_fake" { defaults = { alias = "recovery" } }')),
            ("versions.tf", self.inputs["versions.tf"].replace(b'alias  = "recovery"', b'alias = "unreviewed"')),
            ("main.tf", self.inputs["main.tf"] + b'\nami = "ami-0794a632d5c1058bf"\n'),
        )
        for name, data in cases:
            with self.subTest(name=name):
                path = self.module / name
                path.write_bytes(data)
                with self.assertRaises(guard.QualificationError):
                    self.qualify()
                if name in self.inputs:
                    path.write_bytes(self.inputs[name])
                else:
                    path.unlink()
        (self.module / "outputs.tf").unlink()
        with self.assertRaises(guard.QualificationError):
            self.qualify()
        self.assertEqual(self.test_calls, [])

    def test_nonregular_input_refuses_before_execution(self):
        path = self.module / "outputs.tf"
        path.unlink()
        path.mkdir()
        with self.assertRaises(guard.QualificationError):
            self.qualify()
        self.assertEqual(self.test_calls, [])

    def test_symlink_input_refuses_before_execution(self):
        path = self.module / "outputs.tf"
        path.unlink()
        try:
            path.symlink_to(MODULE / "outputs.tf")
        except OSError as error:
            if os.name != "nt":
                raise
            self.skipTest("host cannot create a symlink fixture: " + str(error))
        with self.assertRaises(guard.QualificationError):
            self.qualify()
        self.assertEqual(self.test_calls, [])


if __name__ == "__main__":
    unittest.main()
