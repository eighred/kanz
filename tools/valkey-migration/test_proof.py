import copy
import json
import unittest
from unittest.mock import patch

import proof


NAME = "kanz-valkey-proof-" + "a" * 24


class MigrationBoundaryTests(unittest.TestCase):
    def test_names_refused_before_commands(self):
        with patch.object(proof, "command") as call:
            for name in ("redis", NAME + "/..", NAME + "-data", "kanz-valkey-proof-" + "a" * 23, NAME.upper()):
                with self.assertRaises(proof.Refused):
                    proof.Fixture(name)
            call.assert_not_called()

    def test_snapshot_detects_lost_or_retimed_claims(self):
        expected = {"a": {"value": "claimed", "expires": 123456}, "b": {"value": "other", "expires": 987654}}
        proof.verify_snapshot(copy.deepcopy(expected), expected)
        for candidate in ({}, {"a": expected["a"]}, {**expected, "c": expected["a"]},
                          {**expected, "a": {"value": "claimed", "expires": 123457}},
                          {**expected, "a": {"value": None, "expires": -2}},
                          {**expected, "b": expected["a"]}):
            with self.assertRaises(proof.Refused):
                proof.verify_snapshot(candidate, expected)

    def test_config_rejects_injection_and_missing_auth(self):
        for password in ("", "a" * 47, "a" * 48 + "\nprotected-mode no"):
            with self.assertRaises(proof.Refused):
                proof.config(password, False)
        self.assertIn("appendfsync always\n", proof.config("a" * 48, False))
        self.assertIn("aof-use-rdb-preamble no\n", proof.config("a" * 48, False))

    def test_fixture_has_no_host_network_port_or_mount(self):
        args = proof.Fixture(NAME).base(proof.REDIS)
        self.assertEqual(args[args.index("--network") + 1], "none")
        self.assertEqual(args[args.index("--mount") + 1], "type=volume,src=" + NAME + "-data,dst=/data")
        self.assertEqual(args[args.index("--user") + 1], "999:999")
        for forbidden in ("--privileged", "-p", "--publish", "--volume", "--env-file"):
            self.assertNotIn(forbidden, args)

    def test_ownership_refuses_wrong_label_image_or_mount(self):
        fixture = proof.Fixture(NAME)
        good = {"Config": {"Labels": {proof.LABEL: NAME}, "Image": proof.REDIS},
                "HostConfig": {"NetworkMode": "none"}, "Image": "sha256:actual",
                "Mounts": [{"Name": NAME + "-data", "Destination": "/data"}]}
        with patch.object(proof, "command", side_effect=[json.dumps([good]), "sha256:actual"]):
            self.assertEqual(fixture.owned(), "sha256:actual")
        bads = []
        for mutate in (lambda x: x["Config"]["Labels"].clear(),
                       lambda x: x["Config"].update(Image="redis:latest"),
                       lambda x: x["HostConfig"].update(NetworkMode="host"),
                       lambda x: x["Mounts"].append({"Destination": "/host"}),
                       lambda x: x.update(Image="sha256:unexpected")):
            item = copy.deepcopy(good)
            mutate(item)
            bads.append(item)
        for item in bads:
            with patch.object(proof, "command", side_effect=[json.dumps([item]), "sha256:actual"]):
                with self.assertRaises(proof.Refused):
                    fixture.owned()

    def test_reopened_claim_is_failure(self):
        fixture = proof.Fixture(NAME)
        with patch.object(fixture, "call", return_value=None):
            fixture.claim_refused("a", 123)
        with patch.object(fixture, "call", return_value="OK"):
            with self.assertRaises(proof.Refused):
                fixture.claim_refused("a", 123)

    def test_cleanup_refuses_unowned_volume(self):
        fixture = proof.Fixture(NAME)
        with patch.object(proof, "command", side_effect=["", fixture.volume, '[{"Labels":{}}]']) as call:
            with self.assertRaises(proof.Refused):
                fixture.cleanup()
            self.assertFalse(any("rm" in entry.args[0] for entry in call.call_args_list))

    def test_evidence_requires_exact_complete_runtime_and_cleanup(self):
        identity = lambda image, version: {"image": image, "version": version, "imageID": "sha256:" + "b" * 64}
        report = {"source": "a" * 40, "verdict": "PASS", "cleanup": "verified_absent", "cases": proof.CASES,
                  "phases": {"redis": identity(proof.REDIS, "7.4.5"), "redis_rollback": identity(proof.REDIS, "7.4.5"),
                             "valkey": identity(proof.VALKEY, "9.1.3"), "valkey_restart": identity(proof.VALKEY, "9.1.3")}}
        proof.validate_report(report, "a" * 40)
        for mutate in (lambda x: x.update(source="c" * 40), lambda x: x.update(cleanup="unknown"),
                       lambda x: x.update(cases=proof.CASES[:-1]), lambda x: x["phases"].pop("redis_rollback"),
                       lambda x: x["phases"]["valkey"].update(image="valkey:latest"),
                       lambda x: x["phases"]["redis_rollback"].update(version="7.9.0")):
            changed = copy.deepcopy(report)
            mutate(changed)
            with self.assertRaises(proof.Refused):
                proof.validate_report(changed, "a" * 40)

    def test_startup_diagnostics_never_return_raw_text(self):
        sentinel = "private-fixture-value-not-for-artifacts"
        for text, category in (("Can't handle RDB format version 12", "unsupported_rdb_version"),
                               ("Permission denied", "filesystem_permission_refused"),
                               ("an unexpected message", "unclassified_startup_refusal")):
            self.assertEqual(proof.startup_category(text + sentinel), category)
            self.assertNotIn(sentinel, proof.startup_category(text + sentinel))

    def test_command_diagnostics_project_only_fixed_category(self):
        sentinel = "private-fixture-value"
        self.assertEqual(proof.command_failure(["docker", "run", sentinel], "unknown log opt max-file " + sentinel),
                         "run:logging_option_refused")
        self.assertEqual(proof.command_failure(["docker", sentinel], sentinel), "other:unclassified_command_failure")


if __name__ == "__main__":
    unittest.main()
