"""Discriminate source, partial-create cleanup and non-secret failure evidence."""
import copy
import json
import subprocess
import tempfile
import traceback
import unittest
from pathlib import Path
from unittest import mock

import qualify

IDENTITY = {"sourceSHA": "a" * 40, "treeSHA": "b" * 40, "runID": "123", "runAttempt": "1"}
OWNER = dict(IDENTITY, schema=1, kind="catalog-qualification", nonce="0123456789abcdef", network="kanz-catalog-0123456789abcdef")


class QualificationControls(unittest.TestCase):
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
                qualify.cleanup_owned(Path("unused"), IDENTITY, execute)
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
            if args[:3] == ["docker", "network", "ls"]:
                return ""
            raise AssertionError("unexpected cleanup command")
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network, fixture.images = OWNER, OWNER["network"], {"gms": "dummy-pinned-image"}
        fixture.run = execute
        with self.assertRaises(ValueError):
            fixture.start("gms", [])
        self.assertIn(name, state)
        with mock.patch.object(qualify, "read_owner", return_value=OWNER):
            qualify.cleanup_owned(Path("unused"), IDENTITY, execute)
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
            if args[:3] == ["docker", "network", "ls"]:
                return ""
            raise AssertionError("foreign container must not authorize mutation")
        with mock.patch.object(qualify, "read_owner", return_value=OWNER), self.assertRaises(ValueError):
            qualify.cleanup_owned(Path("unused"), IDENTITY, execute)
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
            raise AssertionError("unexpected cleanup command")
        with mock.patch.object(qualify, "read_owner", return_value=OWNER):
            qualify.cleanup_owned(Path("unused"), IDENTITY, execute)
        self.assertEqual(deleted, [network_id])
        self.assertFalse(existing)

    def test_secret_argument_timeout_has_only_a_closed_diagnostic(self):
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "system-update", {}
        marker = "one-run-credential-marker"
        command = ["docker", "run", "--env", "PASSWORD=" + marker]
        timeout = subprocess.TimeoutExpired(command, 1)
        with mock.patch.object(qualify.subprocess, "run", side_effect=timeout):
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

    def test_receipt_cannot_pass_with_missing_cases_or_foreign_revision(self):
        images = {"gms": "gms:v1@sha256:" + "1" * 64}
        original = dict(IDENTITY, owner=OWNER, status="passed", cleanupPassed=True,
                        cases=list(qualify.CASES), selectedImages=images, scanFindings=[],
                        images={"gms": {"reference": images["gms"], "imageID": "sha256:" + "2" * 64, "scanSHA256": "3" * 64}},
                        dependencies={"packages": {"acryl-datahub": "1.8.0"}}, projectionTests={"count": 14},
                        qualificationControls={"count": 8}, requiredTests=sorted(qualify.REQUIRED_TESTS),
                        testsSHA256="4" * 64, projectionSHA256="5" * 64, testBinarySHA256="6" * 64)
        qualify.validate_receipt(original, IDENTITY, images)
        for key, value in (("cases", list(qualify.CASES[:-1])), ("sourceSHA", "c" * 40), ("cleanupPassed", False), ("scanFindings", [{"id": "refusal"}]), ("images", {})):
            report = copy.deepcopy(original)
            report[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                qualify.validate_receipt(report, IDENTITY, images)


if __name__ == "__main__":
    unittest.main()
