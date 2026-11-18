import copy
import json
import unittest
from unittest.mock import Mock, call, patch

import proof


NAME = "kanz-valkey-proof-" + "a" * 24


class MigrationBoundaryTests(unittest.TestCase):
    def test_rendered_rig_and_tokyo_keep_separate_cache_selections(self):
        rig = proof.rig_workload()
        root = proof.Path(proof.__file__).resolve().parents[2]
        raw = proof.command(["kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone",
                             str(root / "kanz/infra/overlays/testnet_tokyo/data")])
        workload, = [doc for doc in proof.yaml.safe_load_all(raw)
                     if doc and doc["kind"] == "StatefulSet" and doc["metadata"]["name"] == "redis"]
        tokyo, = workload["spec"]["template"]["spec"]["containers"]
        self.assertEqual(tokyo["image"], proof.REDIS)
        self.assertEqual(rig["image"], proof.VALKEY)
        self.assertIn("exec redis-server --appendonly yes --appendfsync always --maxmemory-policy noeviction", tokyo["command"][2])
        for probe in ("livenessProbe", "readinessProbe"):
            self.assertEqual(tokyo[probe]["exec"]["command"],
                             ["sh", "-c", 'redis-cli -a "$(cat /run/secrets/redis/password)" --no-auth-warning ping'])
        claim, = workload["spec"]["volumeClaimTemplates"]
        self.assertEqual(claim["metadata"]["name"], "data")
        self.assertEqual(claim["spec"]["accessModes"], ["ReadWriteOnce"])
        self.assertEqual(claim["spec"]["resources"]["requests"]["storage"], "1Gi")

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
        self.assertIn("--log-driver=json-file", args)
        self.assertIn("--log-opt=max-size=1m", args)
        self.assertIn("--log-opt=max-file=1", args)
        for forbidden in ("--privileged", "-p", "--publish", "--volume", "--env-file"):
            self.assertNotIn(forbidden, args)

    def test_ownership_refuses_wrong_label_image_or_mount(self):
        fixture = proof.Fixture(NAME)
        good = {"Config": {"Labels": {proof.LABEL: NAME}, "Image": proof.REDIS},
                "HostConfig": {"NetworkMode": "none"}, "Image": "sha256:actual",
                "Mounts": [{"Name": NAME + "-data", "Destination": "/data"}]}
        with patch.object(proof, "command", side_effect=[json.dumps([good]), "sha256:actual"]):
            self.assertEqual(fixture.owned(), good)
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

    def test_first_install_refuses_existing_volume_before_start_and_cleanup(self):
        fixture = proof.Fixture(NAME)
        with patch.object(proof, "command", return_value=fixture.volume) as command:
            with self.assertRaisesRegex(proof.Refused, "fixture_volume_already_exists"):
                fixture.create_volume()
            with self.assertRaisesRegex(proof.Refused, "fixture_volume_already_exists"):
                fixture.cleanup()
            command.assert_called_once_with(["docker", "volume", "ls", "--format", "{{.Name}}"])

    def test_first_install_requires_an_empty_keyspace_across_databases(self):
        for count, keyspace in ((1, "# Keyspace"), (0, "# Keyspace\ndb7:keys=1,expires=1"), (0, "")):
            with self.subTest(count=count, keyspace=keyspace):
                fixture = Mock(spec=proof.Fixture)
                fixture.call.return_value = count
                fixture.raw.return_value = keyspace
                with self.assertRaisesRegex(proof.Refused, "first_install_keyspace_not_empty"):
                    proof.first_install(fixture, {}, {})
                fixture.call.assert_called_once_with("DBSIZE")

    def test_first_install_restarts_valkey_and_rechecks_both_claims(self):
        fixture = Mock(spec=proof.Fixture)
        fixture.start.side_effect = ["fresh", "restarted"]
        fixture.now.side_effect = [1000, 1000, 9000]
        fixture.raw.return_value = "# Keyspace"
        fixture.call.side_effect = [0, "OK", "OK", -2, "OK"]
        a, b = "tenant-a:nonce:first-install", "tenant-b:nonce:first-install"
        retained = {a: {"value": "first-claim", "expires": 901000}}
        fixture.snapshot.side_effect = [
            {**retained, b: {"value": "temporary-claim", "expires": 9000}}, retained,
            {**retained, b: {"value": "new-claim", "expires": 901000}},
        ]
        phases = {}
        proof.first_install(fixture, phases, {})
        self.assertEqual(phases, {"valkey_first_install": "fresh", "first_install_restart": "restarted"})
        self.assertEqual(fixture.start.call_args_list, [call(proof.VALKEY, True, {})] * 2)
        self.assertEqual(fixture.claim_refused.call_args_list, [call(a, 961000)] * 2)
        self.assertEqual(fixture.stop.call_count, 2)
        fixture.cleanup.assert_called_once_with()

    def test_expiry_refuses_lost_or_renewed_claim(self):
        for observed, expected in ((-2, "lost_before_expiry"), (2001, "expiry_changed"), (-1, "expiry_changed")):
            with self.subTest(observed=observed):
                fixture = Mock(spec=proof.Fixture)
                fixture.call.return_value = observed
                fixture.now.return_value = 1999
                with self.assertRaisesRegex(proof.Refused, expected):
                    proof.wait_for_claim_expiry(fixture, "key", 2000)

    def test_evidence_requires_exact_complete_runtime_and_cleanup(self):
        identity = lambda image, version: {"image": image, "version": version, "imageID": "sha256:" + "b" * 64}
        report = {"source": "a" * 40, "verdict": "PASS", "cleanup": "verified_absent", "cases": proof.CASES,
                  "phases": {"redis": identity(proof.REDIS, "7.4.5"), "redis_rollback": identity(proof.REDIS, "7.4.5"),
                             **{phase: identity(proof.VALKEY, "9.1.3") for phase in
                                ("valkey", "valkey_restart", "valkey_first_install", "first_install_restart")}}}
        for i, (name, phase) in enumerate(report["phases"].items(), start=1):
            phase.update(containerID=f"{i:064x}", serverRunID=f"{i:040x}",
                         volumeGeneration=("c" if "first_install" in name else "d") * 32)
        proof.validate_report(report, "a" * 40)
        for mutate in (lambda x: x.update(source="c" * 40), lambda x: x.update(cleanup="unknown"),
                       lambda x: x.update(cases=proof.CASES[:-1]), lambda x: x["phases"].pop("redis_rollback"),
                       lambda x: x["phases"]["valkey"].update(image="valkey:latest"),
                       lambda x: x["phases"].pop("valkey_first_install"),
                       lambda x: x["phases"].update(valkey_restart=x["phases"]["valkey"]),
                       lambda x: x["phases"]["redis_rollback"].update(serverRunID=x["phases"]["redis"]["serverRunID"]),
                       lambda x: x["phases"]["first_install_restart"].update(volumeGeneration="d" * 32),
                       lambda x: x["phases"]["valkey_restart"].update(volumeGeneration="e" * 32),
                       lambda x: x["phases"]["first_install_restart"].update(imageID="sha256:" + "c" * 64),
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
