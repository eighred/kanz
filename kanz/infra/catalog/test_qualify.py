"""Discriminate source, partial-create cleanup and non-secret failure evidence."""
import copy
import fnmatch
import json
import os
import subprocess
import tempfile
import traceback
import unittest
from pathlib import Path
from unittest import mock

import qualify
import yaml

IDENTITY = {"sourceSHA": "a" * 40, "treeSHA": "b" * 40, "runID": "123", "runAttempt": "1"}
OWNER_PATH = Path("/owned/run/catalog-owner.json")
OWNER = dict(IDENTITY, schema=1, kind="catalog-qualification", nonce="0123456789abcdef", network="kanz-catalog-0123456789abcdef",
             scannerMount={"path": "/owned/run/catalog-fixture/scanner-state", "source": "kanz-catalog-0123456789abcdef-scanner", "filesystem": "tmpfs", "bytes": qualify.SCANNER_DISK_LIMIT, "options": ["nosuid", "nodev", "noexec"], "uid": os.getuid(), "gid": os.getgid()})
OWNER["derivedImage"] = qualify.image_patch.owned_spec(OWNER, OWNER_PATH)


class QualificationControls(unittest.TestCase):
    def test_consumed_inputs_select_both_pr_and_main_qualification(self):
        path = Path(os.environ.get("KANZ_CATALOG_WORKFLOW", qualify.ROOT / ".github/workflows/catalog-verification.yml"))
        # BaseLoader retains the Actions `on` key rather than YAML 1.1's bool.
        workflow = yaml.load(path.read_text(), Loader=yaml.BaseLoader)
        consumed = ("kanz-jl/qualification/install-audit.sh", "kanz/pkg/auth/audit.go",
                    "kanz/pkg/authbus/recorder.go", "kanz/pkg/bus/validate.go",
                    "kanz/pkg/decisionq/queue.go", "kanz/pkg/observability/observability.go",
                    "kanz/pkg/transport/tls.go", "kanz/internal/bustest/bustest.go",
                    "kanz/internal/env/env.go", "kanz/internal/lifecycle/fatal.go",
                    "kanz/internal/platform/httpserver/httpserver.go", "kanz/internal/version/version.go",
                    "kanz/test/arch/upstream_principal_reconstruction_test.go",
                    "kanz-schemas/proto/envelope/v1/envelope.proto",
                    "kanz-schemas/proto/observation/v1/observation.proto",
                    "kanz-schemas/proto/common/v1/decimal.proto", "kanz/go.mod", "kanz/go.sum",
                    "kanz/Makefile", "kanz-schemas/buf.yaml", "kanz-schemas/buf.gen.yaml",
                    "kanz-schemas/bootstrap-go-sdk.sh", ".github/actions/setup-buf/action.yml")
        def validate(document):
            for event in ("pull_request", "push"):
                patterns = document["on"][event]["paths"]
                if any(not any(fnmatch.fnmatchcase(source, pattern) for pattern in patterns) for source in consumed):
                    raise ValueError("consumed qualification input is unselected")
                if any(any(fnmatch.fnmatchcase(source, pattern) for pattern in patterns) for source in ("README.md", "tools/grafana_proof/proof.py")):
                    raise ValueError("unrelated source unnecessarily selects catalog runtime")
        validate(workflow)
        for event in ("pull_request", "push"):
            for source in (consumed[0], consumed[1], "kanz-schemas/proto/envelope/v1/envelope.proto"):
                changed = copy.deepcopy(workflow)
                changed["on"][event]["paths"] = [pattern for pattern in changed["on"][event]["paths"] if not fnmatch.fnmatchcase(source, pattern)]
                with self.subTest(event=event, source=source), self.assertRaises(ValueError):
                    validate(changed)

    def test_independent_source_and_clean_tree_are_required(self):
        expected = {key: value for key, value in IDENTITY.items() if key != "treeSHA"}
        run = mock.Mock(side_effect=[IDENTITY["sourceSHA"], IDENTITY["treeSHA"], ""])
        self.assertEqual(qualify.source_identity(run, expected), IDENTITY)
        for head, tree, dirty in (("c" * 40, "b" * 40, ""), ("a" * 40, "b" * 40, " M projection.py"), ("a" * 40, "main", "")):
            with self.subTest(head=head, tree=tree, dirty=dirty), self.assertRaises(ValueError):
                qualify.source_identity(mock.Mock(side_effect=[head, tree, dirty]), expected)

    def test_owner_creation_is_exclusive_and_does_not_follow_links(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owner.json"
            qualify.write_owner(path, OWNER)
            self.assertEqual(qualify.read_owner(path), OWNER)
            with self.assertRaises(FileExistsError):
                qualify.write_owner(path, OWNER)
            link = Path(directory) / "link.json"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                qualify.read_owner(link)

    def test_stale_or_foreign_owner_cannot_authorize_docker(self):
        for key, value in (("sourceSHA", "c" * 40), ("treeSHA", "c" * 40), ("runID", "999"), ("runAttempt", "2"), ("nonce", "fedcba9876543210")):
            owner = dict(OWNER, **{key: value})
            execute = mock.Mock()
            with self.subTest(key=key), mock.patch.object(qualify, "read_owner", return_value=owner), self.assertRaises(ValueError):
                qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
            execute.assert_not_called()

    def test_failed_start_still_removes_the_created_owned_container_by_id(self):
        state = {}
        deleted = []
        name = OWNER["network"] + "-gms"
        container_id = "1" * 64
        def execute(args, **_kwargs):
            if args[:3] == ["docker", "run", "--detach"]:
                state[name] = {"Id": container_id, "Config": {"Labels": qualify.labels(OWNER)}}
                raise ValueError("start failed after create")
            if args[:3] == ["docker", "ps", "--all"]:
                if "name=^/" + name + "$" in args and name in state:
                    return name
                return ""
            if args[:3] == ["docker", "inspect", "--type"]:
                return json.dumps([state[args[-1]]])
            if args[:3] == ["docker", "rm", "--force"]:
                self.assertEqual(args[-1], container_id)
                deleted.append(args[-1])
                state.clear()
                return ""
            if args[:3] in (["docker", "network", "ls"], ["docker", "image", "ls"]):
                return ""
            raise AssertionError("unexpected cleanup command")
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network, fixture.images = OWNER, OWNER["network"], {"gms": "dummy-pinned-image"}
        fixture.run = execute
        with self.assertRaises(ValueError):
            fixture.start("gms", [])
        self.assertIn(name, state)
        with mock.patch.object(qualify, "read_owner", return_value=OWNER):
            qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
        self.assertFalse(state)
        self.assertEqual(deleted, [container_id])

    def test_foreign_name_collision_is_never_deleted(self):
        name = OWNER["network"] + "-gms"
        calls = []
        def execute(args, **_kwargs):
            calls.append(args)
            if args[:3] == ["docker", "ps", "--all"]:
                return name if "name=^/" + name + "$" in args else ""
            if args[:3] == ["docker", "inspect", "--type"]:
                return json.dumps([{"Id": "1" * 64, "Config": {"Labels": {"kanz.catalog.owner": "foreign"}}}])
            if args[:3] in (["docker", "network", "ls"], ["docker", "image", "ls"]):
                return ""
            raise AssertionError("foreign container must not authorize mutation")
        with mock.patch.object(qualify, "read_owner", return_value=OWNER), self.assertRaises(ValueError):
            qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
        self.assertFalse(any(args[:2] == ["docker", "rm"] for args in calls))

    def test_partial_network_create_is_cleaned_using_the_same_owner(self):
        existing = True
        deleted = []
        network_id = "2" * 64
        def execute(args, **_kwargs):
            nonlocal existing
            if args[:3] == ["docker", "ps", "--all"]:
                return ""
            if args[:3] == ["docker", "network", "ls"]:
                return OWNER["network"] if existing else ""
            if args[:3] == ["docker", "network", "inspect"]:
                return json.dumps([{"Id": network_id, "Labels": qualify.labels(OWNER)}])
            if args[:3] == ["docker", "network", "rm"]:
                deleted.append(args[-1])
                existing = False
                return ""
            if args[:3] == ["docker", "image", "ls"]:
                return ""
            raise AssertionError("unexpected cleanup command")
        with mock.patch.object(qualify, "read_owner", return_value=OWNER):
            qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
        self.assertEqual(deleted, [network_id])
        self.assertFalse(existing)

    def test_secret_argument_timeout_has_only_a_closed_diagnostic(self):
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "system-update", {}
        marker = "one-run-credential-marker"
        command = ["docker", "run", "--env", "PASSWORD=" + marker]
        timeout = subprocess.TimeoutExpired(command, 1)
        with mock.patch.object(qualify.subprocess, "Popen", side_effect=timeout):
            try:
                fixture.run(command)
            except ValueError as error:
                diagnostic = str(error) + traceback.format_exc()
            else:
                self.fail("timeout must refuse")
        self.assertIn("system-update", diagnostic)
        self.assertNotIn(marker, diagnostic)
        self.assertNotIn("PASSWORD=", diagnostic)
        self.assertNotIn(str(command), diagnostic)

    def test_output_overflow_is_bounded_and_terminates_the_process_group(self):
        output = bytearray(b"a" * qualify.OUTPUT_LIMIT)
        with self.assertRaises(qualify.CommandFailure):
            qualify.append_output(output, b"x")
        self.assertEqual(len(output), qualify.OUTPUT_LIMIT)
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "bounded-command", {}
        for category in ("output-budget", "timeout"):
            for parent_code in (None, 0, 7):
                for stop_failure in (False, True):
                    process = mock.Mock()
                    process.poll.return_value = parent_code
                    failure = qualify.CommandFailure("reap", "reap-timeout") if stop_failure else None
                    with self.subTest(category=category, parent_code=parent_code, stop_failure=stop_failure), mock.patch.object(qualify.subprocess, "Popen", return_value=process) as launch, mock.patch.object(qualify, "capture_process", side_effect=qualify.CommandFailure("capture", category)), mock.patch.object(qualify, "stop_process", side_effect=failure) as stop:
                        with self.assertRaises(qualify.CommandFailure) as refused:
                            fixture.run(["dummy-command"])
                        self.assertEqual(refused.exception.category, "reap-timeout" if stop_failure else category)
                        stop.assert_called_once_with(process)
                        process.stdout.close.assert_called_once_with()
                        self.assertEqual(launch.call_args.kwargs["stdout"], subprocess.PIPE)
                        self.assertTrue(launch.call_args.kwargs["start_new_session"])
        process = mock.Mock(pid=1234)
        with mock.patch.object(qualify.os, "killpg") as terminate:
            qualify.stop_process(process)
        terminate.assert_called_once_with(1234, qualify.signal.SIGKILL)
        process.wait.assert_called_once_with(timeout=10)

    def test_scanner_profile_preserves_finite_default_command_limits(self):
        self.assertEqual(qualify.command_limits(["trivy"], True), 8 << 30)
        self.assertEqual(qualify.command_limits(["docker"], False), 8 << 20)
        with self.assertRaises(ValueError):
            qualify.command_limits(["docker"], True)
        self.assertEqual(qualify.SCANNER_DISK_LIMIT, 12 << 30)
        for layer in qualify.REFERENCE_DB_LAYERS:
            self.assertGreater(layer["compressedBytes"], qualify.DEFAULT_FILE_LIMIT)
            self.assertLess(layer["compressedBytes"], qualify.SCANNER_FILE_LIMIT)
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "resource-profile", {}
        for args, scanner, expected in ((["trivy"], True, 8 << 30), (["docker"], False, 8 << 20)):
            process = mock.Mock()
            process.poll.return_value = 0
            with mock.patch.object(qualify.subprocess, "Popen", return_value=process) as launch, mock.patch.object(qualify, "capture_process", return_value=(0, b"")), mock.patch.object(qualify.resource, "setrlimit") as limits:
                fixture.run(args, scanner=scanner)
                launch.call_args.kwargs["preexec_fn"]()
                limits.assert_any_call(qualify.resource.RLIMIT_FSIZE, (expected, expected))

    def test_scanner_failure_categories_keep_refusals_distinct(self):
        cases = ((b"copy error: file too large", 1, "scanner-file-budget"),
                 (b"no space left on device", 1, "scanner-disk-budget"),
                 (b"dial tcp: no such host", 1, "scanner-network"),
                 (b"Java DB download failed", 1, "scanner-java-db-download"),
                 (b"vulnerability database download failed", 1, "scanner-db-download"),
                 (b"unknown refusal", 1, "scanner-command-exit"))
        for body, code, expected in cases:
            with self.subTest(expected=expected):
                self.assertEqual(qualify.failure_category(body, code, True), expected)

    def test_scanner_cleanup_requires_exact_owned_mount_identity(self):
        mount = {"filesystem": "tmpfs", "source": OWNER["scannerMount"]["source"], "options": ["rw", "nosuid", "nodev", "noexec"]}
        execute = mock.Mock()
        with mock.patch.object(qualify, "mount_record", side_effect=[mount, None]), mock.patch.object(qualify.Path, "stat", return_value=mock.Mock(st_uid=os.getuid(), st_gid=os.getgid())), mock.patch.object(qualify.os, "statvfs", return_value=mock.Mock(f_frsize=4096, f_blocks=qualify.SCANNER_DISK_LIMIT // 4096)):
            qualify.cleanup_scanner(OWNER, OWNER_PATH, execute)
        execute.assert_called_once_with(["sudo", "-n", "umount", "--", OWNER["scannerMount"]["path"]])
        execute.reset_mock()
        with mock.patch.object(qualify, "mount_record", return_value=dict(mount, source="foreign")), self.assertRaises(ValueError):
            qualify.cleanup_scanner(OWNER, OWNER_PATH, execute)
        execute.assert_not_called()
        changed = copy.deepcopy(OWNER)
        changed["scannerMount"]["path"] = "/foreign/path"
        with self.assertRaises(ValueError):
            qualify.cleanup_scanner(changed, OWNER_PATH, execute)
        execute.assert_not_called()

    def test_receipt_cannot_pass_with_missing_cases_or_foreign_revision(self):
        images = {"gms": "gms:v1@sha256:" + "1" * 64}
        original = dict(IDENTITY, owner=OWNER, status="passed", cleanupPassed=True,
                        cases=list(qualify.CASES), selectedImages=images, scanFindings=[],
                        images={"gms": {"reference": images["gms"], "imageID": "sha256:" + "2" * 64, "scanSHA256": "3" * 64, "scanVerdict": "passed"}},
                        dependencies={"packages": {"acryl-datahub": "1.8.0"}}, projectionTests={"count": 14},
                        qualificationControls={"count": 24}, requiredTests=sorted(qualify.REQUIRED_TESTS),
                        testsSHA256="4" * 64, projectionSHA256="5" * 64, testBinarySHA256="6" * 64)
        original["scanner"] = {"scanner": "trivy-0.72.0", "fileLimitBytes": qualify.SCANNER_FILE_LIMIT, "cacheTempLimitBytes": qualify.SCANNER_DISK_LIMIT, "stdoutLimitBytes": qualify.OUTPUT_LIMIT, "defaultFileLimitBytes": qualify.DEFAULT_FILE_LIMIT, "commandTimeoutSeconds": 600, "releasedBeforeRuntime": True, "advisoryRoles": [], "advisoryExitCode": qualify.image_patch.ADVISORY_EXIT, "referenceObservedLayers": qualify.REFERENCE_DB_LAYERS,
                               "databases": {name: {"metadata": {"Version": version}, "metadataSHA256": "a" * 64, "database": {"bytes": 100, "sha256": "b" * 64}} for name, version in (("db", 2), ("java-db", 1))}}
        qualify.validate_receipt(original, IDENTITY, images)
        for key, value in (("cases", list(qualify.CASES[:-1])), ("sourceSHA", "c" * 40), ("cleanupPassed", False), ("scanFindings", [{"id": "refusal"}]), ("images", {}), ("scanner", {}), ("failure", {"category": "refused"})):
            report = copy.deepcopy(original)
            report[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                qualify.validate_receipt(report, IDENTITY, images)


if __name__ == "__main__":
    unittest.main()
