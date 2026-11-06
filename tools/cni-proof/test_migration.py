"""Source-side discriminators for the disposable transition fixture."""
import copy
import json
import unittest
from unittest.mock import patch

import migration
import proof


class MigrationTests(unittest.TestCase):
    def test_service_rule_readiness_requires_chain_and_nonempty_save(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        with patch.object(f, "inside", side_effect=["version", "version", proof.ProofError("chain_absent"), "", "", "", "*nat\nCOMMIT"]) as call, \
                patch.object(migration.time, "sleep") as sleep:
            f.service_rules_ready()
            self.assertEqual(sleep.call_count, 2)
            self.assertEqual(call.call_args_list[-2].args, ("iptables", "-t", "nat", "-S", "KUBE-SERVICES"))
            self.assertEqual(call.call_args_list[-1].args, ("iptables-save",))

    def test_service_rule_readiness_timeout_never_mutates_rules(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        with patch.object(f, "inside", return_value="") as call, \
                patch.object(migration.time, "monotonic", side_effect=[0, 0, 121]), \
                patch.object(migration.time, "sleep"):
            with self.assertRaisesRegex(proof.ProofError, "^service_rules_not_ready$"):
                f.service_rules_ready()
            self.assertTrue(all("iptables-restore" not in item.args for item in call.call_args_list))
        with patch.object(f, "inside", side_effect=proof.ProofError("missing_binary")) as call:
            with self.assertRaisesRegex(proof.ProofError, "^missing_binary$"):
                f.service_rules_ready()
            self.assertEqual(call.call_count, 1)

    def test_bpffs_propagation_is_only_new_mount(self):
        before = {"1": ("/", "overlay", frozenset()), "2": ("/sys", "sysfs", frozenset()),
                  "3": ("/sys/fs", "tmpfs", frozenset())}
        mounted = {**before, "4": ("/sys/fs/bpf", "bpf", frozenset())}
        self.assertEqual(migration.private_mount_boundary(before, mounted), "4")
        shared = {**mounted, "4": ("/sys/fs/bpf", "bpf", frozenset({"shared:99"}))}
        self.assertEqual(migration.private_mount_boundary(before, shared, "4"), "4")
        for identity in before:
            changed = {**before, identity: (before[identity][0], before[identity][1], frozenset({"master:77"}))}
            with self.assertRaises(proof.ProofError):
                migration.private_mount_boundary(changed)
        for changed in (
                {**shared, "3": ("/sys/fs", "tmpfs", frozenset({"shared:77"}))},
                {**shared, "5": ("/unrelated", "tmpfs", frozenset({"shared:77"}))},
                {**mounted, "4": ("/sys/fs/bpf", "bpf", frozenset({"shared:99", "master:77"}))},
                {key: value for key, value in shared.items() if key != "3"}):
            with self.assertRaises(proof.ProofError):
                migration.private_mount_boundary(before, changed, "4")
        with self.assertRaises(proof.ProofError):
            migration.private_mount_boundary(mounted)
        with self.assertRaises(proof.ProofError):
            migration.private_mount_boundary(before, shared, "different-mount")

    def test_cgroup_mount_is_new_private_and_fixed_target(self):
        before = {"1": ("/", "overlay", frozenset()), "2": ("/sys", "sysfs", frozenset()),
                  "3": ("/run", "tmpfs", frozenset()), "4": ("/run/cilium", "tmpfs", frozenset()),
                  "5": ("/sys/fs/bpf", "bpf", frozenset({"shared:55"}))}
        options = {"target": "/run/cilium/cgroupv2", "filesystem": "cgroup2"}
        mounted = {**before, "6": (options["target"], "cgroup2", frozenset())}
        shared = {**mounted, "6": (options["target"], "cgroup2", frozenset({"shared:66"}))}
        self.assertEqual(migration.private_mount_boundary(before, mounted, **options), "6")
        self.assertEqual(migration.private_mount_boundary(before, shared, "6", **options), "6")
        for changed in (mounted, {**before, "4": ("/run/cilium", "tmpfs", frozenset({"shared:44"}))}):
            with self.assertRaises(proof.ProofError):
                migration.private_mount_boundary(changed, **options)
        for changed in ({**shared, "3": ("/run", "tmpfs", frozenset({"shared:33"}))},
                        {**shared, "6": (options["target"], "cgroup2", frozenset({"shared:66", "master:9"}))},
                        {**mounted, "6": (options["target"], "tmpfs", frozenset())}):
            with self.assertRaises(proof.ProofError):
                migration.private_mount_boundary(before, changed, "6", **options)
        for target, filesystem in (("/sys/fs/cgroup", "cgroup2"), ("/", "cgroup2"), (options["target"], "tmpfs")):
            with self.assertRaisesRegex(proof.ProofError, "unapproved_fixture_mount"):
                migration.private_mount_boundary(before, target=target, filesystem=filesystem)

    def test_policy_cleanup_requires_successful_nonempty_save(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        for result in ("", proof.ProofError("failed")):
            with patch.object(f, "inside", side_effect=[result]) as call:
                with self.assertRaises(proof.ProofError):
                    f.remove_old_policy_rules()
                self.assertEqual(call.call_count, 1)

    def test_no_old_policy_does_not_restore_other_rules(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        with patch.object(f, "inside", return_value="*filter\n:INPUT ACCEPT [0:0]\nCOMMIT") as call:
            f.remove_old_policy_rules()
            self.assertEqual(call.call_count, 1)
            self.assertEqual(f.rule_diagnostic["status"], "no_old_rules")

    def test_old_policy_cleanup_checks_restore_and_rechecks_absence(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        original = "*filter\n:INPUT ACCEPT [0:0]\n:KUBE-ROUTER - [0:0]\n-A INPUT -j KUBE-ROUTER\nCOMMIT"
        expected = "*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n"
        with patch.object(f, "inside", side_effect=[original, "", "", expected]) as call:
            f.remove_old_policy_rules()
            self.assertEqual(call.call_args_list[1].args, ("iptables-restore", "--test"))
            self.assertEqual(call.call_args_list[1].kwargs, {"data": expected})
            self.assertEqual(call.call_args_list[2].args, ("iptables-restore",))
            self.assertEqual(f.rule_diagnostic["status"], "removed_verified")

    def test_cpu_bound_follows_host_and_caps_at_four(self):
        for available, expected in ((None, 1), (1, 1), (2, 2), (16, 4)):
            with patch.object(migration.os, "cpu_count", return_value=available):
                self.assertEqual(migration.cpu_limit(), "--cpus=" + str(expected))

    def test_target_is_generated_fixture_only(self):
        for name in ("production", "kanz-cni-migration-", "../node", "kanz-cni-migration-" + "0" * 25):
            with self.assertRaises(proof.ProofError):
                migration.Fixture(name)
        migration.Fixture("kanz-cni-migration-" + "a" * 24)

    def test_no_host_mount_or_wrong_image_or_label(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        valid = {"Config": {"Labels": {migration.LABEL: f.name}, "Image": migration.NODE},
                 "HostConfig": {"CgroupnsMode": "private"},
                 "Mounts": [{"Type": "volume"}], "Id": "fixed", "Image": "sha256:fixture"}
        with patch.object(migration, "command", side_effect=[json.dumps([valid]), "sha256:fixture"]):
            self.assertEqual(f.ownership(), "fixed")
        for mutate in (lambda x: x["Config"].update(Image="latest"),
                       lambda x: x["Config"].update(Labels={}),
                       lambda x: x["HostConfig"].update(PidMode="host"),
                       lambda x: x["HostConfig"].update(CgroupnsMode="host"),
                       lambda x: x["HostConfig"].update(NetworkMode="host"),
                       lambda x: x["Mounts"].append({"Type": "bind"})):
            obj = copy.deepcopy(valid)
            mutate(obj)
            with patch.object(migration, "command", side_effect=[json.dumps([obj]), "sha256:fixture"]):
                with self.assertRaises(proof.ProofError):
                    f.ownership()

    def test_configuration_preserves_kube_proxy(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        with patch.object(f, "ownership"), patch.object(f, "inside") as call:
            f.configure(True)
            enabled = json.loads(call.call_args.kwargs["data"])
            self.assertEqual(enabled["flannel-backend"], "none")
            self.assertTrue(enabled["disable-network-policy"])
            self.assertNotIn("disable-kube-proxy", enabled)
            f.configure(False)
            self.assertEqual(json.loads(call.call_args.kwargs["data"]), migration.BASE_CONFIG)

    def test_command_failure_does_not_disclose_output(self):
        result = type("Result", (), {"returncode": 1, "stdout": "fixture-secret", "stderr": "fixture-secret"})()
        with patch.object(migration.subprocess, "run", return_value=result):
            with self.assertRaisesRegex(proof.ProofError, "^fixture_command_failed$"):
                migration.command(["fixture"])

    def test_cilium_phase_rejects_old_flannel_even_if_policy_would_pass(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        f.mode = "cilium"
        with patch.object(f, "inside", side_effect=[migration.CNI_DIR + "/10-flannel.conflist",
                                                   json.dumps({"plugins": [{"type": "flannel"}]})]):
            with self.assertRaisesRegex(proof.ProofError, "Cilium_not_active_CNI"):
                f.datapath()

    def test_rollback_rejects_remaining_cilium(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        with patch.object(f, "inside", side_effect=[migration.CNI_DIR + "/10-flannel.conflist",
                                                   json.dumps({"plugins": [{"type": "flannel"}]})]), \
                patch.object(f, "k", return_value="daemonset.apps/cilium"):
            with self.assertRaisesRegex(proof.ProofError, "Cilium_remains_after_rollback"):
                f.datapath()

    def test_storage_diagnostic_projects_only_finite_status(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        pod = {"spec": {"secret": "not-retained"}, "status": {"phase": "Failed", "message": "not-retained",
               "containerStatuses": [{"state": {"terminated": {"reason": "Error", "exitCode": 1, "message": "not-retained"}}}]}}
        with patch.object(f, "k", return_value=json.dumps(pod)):
            self.assertEqual(f.storage_diagnostic(), {"pod": "Failed", "containers": [{"reason": "Error", "exitCode": 1}]})

    def test_packaged_controller_creation_precedes_rollout_wait(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        with patch.object(f, "k", side_effect=["", "deployment/coredns", "ready", "deployment/local-path-provisioner", "ready"]) as call, \
                patch.object(migration.time, "sleep"):
            f.initial_system_ready()
            self.assertEqual(call.call_args_list[0].args[2], "get")
            self.assertEqual(call.call_args_list[1].args[2], "get")
            self.assertEqual(call.call_args_list[2].args[2], "rollout")

    def test_cilium_diagnostic_omits_logs_and_arbitrary_values(self):
        f = migration.Fixture("kanz-cni-migration-" + "a" * 24)
        value = {"items": [{"spec": {"token": "not-retained"}, "status": {
            "initContainerStatuses": [{"name": "unexpected-private-value", "state": {
                "terminated": {"reason": "unexpected-private-value", "message": "not-retained", "exitCode": 7}}}]}}]}
        with patch.object(f, "k", return_value=json.dumps(value)):
            self.assertEqual(f.cilium_diagnostic(), [[{"name": "Other", "reason": "Other", "diagnostic": "unknown", "exitCode": 7, "running": False, "ready": False}]])

    def test_runtime_diagnostic_is_bounded_closed_projection(self):
        cases = ((None, "unknown"), ({"token": "private"}, "unknown"),
                 ("private-token", "unknown"), ("apparmor" + "x" * 4096, "unknown"),
                 ("path /sys/fs/bpf is not a shared mount: private", "bpffs_not_shared"),
                 ("path /run/cilium/cgroupv2 is not a shared or slave mount: private", "cgroup_not_shared"),
                 ("path /var/run/cilium/netns is not a shared mount: private", "netns_not_shared"),
                 ("path /private is not a shared mount", "mount_not_shared"),
                 ("AppArmor profile private refused", "apparmor_refused"),
                 ("seccomp private refused", "seccomp_refused"),
                 ("private: read-only file system", "read_only_filesystem"),
                 ("private: permission denied", "permission_denied"),
                 ("private: operation not permitted", "operation_not_permitted"))
        for message, expected in cases:
            self.assertEqual(migration.runtime_diagnostic(message), expected)


if __name__ == "__main__":
    unittest.main()
