import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import proof


class IdentityFixtureTests(unittest.TestCase):
    def test_broker_mount_preserves_entire_canonical_acl(self):
        source = proof.canonical_tenancy()
        docs = proof.broker_objects()
        mounted = [d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-tenants"]
        self.assertEqual(mounted, [source])
        body = mounted[0]["data"]["tenants.conf"]
        self.assertTrue(body.endswith("\n"))
        self.assertIn("system_account: SYS", body)
        self.assertIn("__system__", body)
        self.assertIn("tenant.*.order.order.submit", body)
        self.assertNotIn("FIXTURE:", body)
        config = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "nats-config")
        for invariant in ("verify: true", "verify_and_map: true", 'include "tenants.conf"'):
            self.assertIn(invariant, config["data"]["nats.conf"])
        self.assertNotIn("cluster {", config["data"]["nats.conf"])

    def test_duplicate_or_unregistered_canonical_identity_is_refused(self):
        source = proof.canonical_tenancy()
        for changed in (source["data"]["tenants.conf"] + "agent-identity-unregistered",
                        source["data"]["tenants.conf"].replace("sa/copilot", "sa/other")):
            mutant = copy.deepcopy(source)
            mutant["data"]["tenants.conf"] = changed
            with patch.object(proof.FEED, "manifests", return_value=[mutant]), self.assertRaises(ValueError):
                proof.canonical_tenancy()

    def test_grant_mutation_changes_only_one_private_copy(self):
        source = proof.canonical_tenancy()
        saved = copy.deepcopy(source)
        mutant = proof.widen_copy(source)
        self.assertEqual(source, saved)
        self.assertNotEqual(mutant["metadata"]["name"], source["metadata"]["name"])
        expected = '\n                  ">",\n                '
        self.assertEqual(mutant["data"]["tenants.conf"].replace(expected, "", 1), source["data"]["tenants.conf"])
        self.assertEqual(mutant["data"]["tenants.conf"].count(expected), 1)
        bad = copy.deepcopy(source)
        bad["data"]["tenants.conf"] = bad["data"]["tenants.conf"].replace("sa/copilot", "sa/copilot-other")
        with self.assertRaises(ValueError):
            proof.widen_copy(bad)

    def test_mutant_retains_valid_broker_identity_and_own_acl(self):
        docs = proof.broker_objects()
        saved = copy.deepcopy(docs)
        mutant = proof.mutant_broker(docs)
        self.assertEqual(docs, saved)
        self.assertEqual(mutant["metadata"]["name"], "nats-mutant")
        pod = mutant["spec"]["template"]
        self.assertEqual(pod["spec"]["serviceAccountName"], "nats")
        mounts = [s["configMap"]["name"] for v in pod["spec"]["volumes"] for s in v.get("projected", {}).get("sources", []) if "configMap" in s]
        self.assertEqual(mounts, ["nats-config", "agent-identity-mutant-tenants"])
        self.assertNotIn("volumeClaimTemplates", mutant["spec"])
        self.assertEqual(mutant["spec"]["replicas"], 1)

    def test_job_mounts_workload_api_without_identity_material(self):
        job = proof.probe_job("fixture", "kanz-services", "copilot", "denied", "10.96.0.2:4222", "private-fixture:owned")
        pod = job["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(pod["serviceAccountName"], "copilot")
        self.assertEqual(pod["volumes"], [{"name": "spiffe", "csi": {"driver": "csi.spiffe.io", "readOnly": True}}])
        self.assertTrue(pod["containers"][0]["securityContext"]["readOnlyRootFilesystem"])
        self.assertFalse(pod["containers"][0]["securityContext"]["allowPrivilegeEscalation"])
        self.assertNotIn("secret", json.dumps(job).lower())
        self.assertEqual(job["spec"]["activeDeadlineSeconds"], 90)
        recorder = proof.probe_job("recorder", "kanz-services", "copilot", "wrong-server", "10.96.0.2:4222", "private-fixture:owned", recorder=True)
        self.assertEqual(recorder["spec"]["template"]["spec"]["containers"][0]["command"][0], "/copilot.test")

    def test_probe_network_policy_is_bounded_in_both_namespaces(self):
        documents = proof.probe_policies()
        proof.validate_probe_policies(documents)
        self.assertEqual({d["metadata"]["namespace"] for d in documents}, {"kanz-services", "kanz-messaging"})
        for document in documents:
            spec = document["spec"]
            self.assertEqual(set(spec["policyTypes"]), {"Ingress", "Egress"})
            self.assertEqual(spec["podSelector"], {"matchLabels": {"agent-identity-probe": "true"}})
            self.assertEqual(spec["ingress"], [])
            self.assertEqual(len(spec["egress"]), 1)
            rule = spec["egress"][0]
            self.assertEqual(rule["ports"], [{"protocol": "TCP", "port": 4222}])
            self.assertEqual(rule["to"], [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kanz-messaging"}},
                                          "podSelector": {"matchLabels": {"identity-proof-broker": "true"}}}])

    def test_removed_or_broadened_probe_isolation_is_refused(self):
        documents = proof.probe_policies()
        with self.assertRaises(ValueError):
            proof.validate_probe_policies(documents[:1])
        changes = [
            lambda d: d["spec"].update(podSelector={}),
            lambda d: d["spec"].update(policyTypes=["Ingress"]),
            lambda d: d["spec"].update(ingress=[{}]),
            lambda d: d["spec"].update(egress=[{}]),
            lambda d: d["spec"]["egress"][0]["to"][0].update(namespaceSelector={}),
            lambda d: d["spec"]["egress"][0]["to"][0].update(podSelector={}),
            lambda d: d["spec"]["egress"][0]["ports"][0].update(port=443),
            lambda d: d["spec"]["egress"].append({"to": [{"ipBlock": {"cidr": "0.0.0.0/0"}}]}),
        ]
        for namespace_index in (0, 1):
            for change in changes:
                mutant = copy.deepcopy(documents)
                change(mutant[namespace_index])
                with self.assertRaises(ValueError):
                    proof.validate_probe_policies(mutant)

    def test_inert_network_controls_have_no_external_egress(self):
        documents = proof.network_controls("private-fixture:owned")
        pod = next(d for d in documents if d["kind"] == "Pod")
        self.assertFalse(pod["spec"]["automountServiceAccountToken"])
        self.assertNotIn("volumes", pod["spec"])
        policies = [d for d in documents if d["kind"] == "NetworkPolicy"]
        inert = next(d for d in policies if d["metadata"]["name"] == "agent-identity-inert")
        self.assertEqual(inert["spec"]["egress"], [])
        controls = [d for d in policies if d["metadata"]["name"] == "agent-identity-network-control"]
        self.assertEqual({d["metadata"]["namespace"] for d in controls}, {"kanz-services", "kanz-messaging"})
        for control in controls:
            rule = control["spec"]["egress"][0]
            self.assertEqual(rule["to"], [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kanz-services"}},
                                          "podSelector": {"matchLabels": {"agent-identity-inert": "true"}}}])
            self.assertEqual(rule["ports"], [{"protocol": "TCP", "port": 4222}])

    def test_result_requires_one_exact_marker(self):
        raw = proof.MARKER + json.dumps({"passed": True, "cases": {"copilot_svid_issued": True}})
        self.assertTrue(proof.receipt("unretained log\n" + raw)["passed"])
        for changed in ("", raw + "\n" + raw, proof.MARKER + "{}", proof.MARKER + "not-json"):
            with self.assertRaises((ValueError, KeyError)):
                proof.receipt(changed)

    def test_command_environment_discards_ambient_authority(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "AWS_SECRET_ACCESS_KEY": "not-a-secret-fixture", "KUBECONFIG": "foreign-context",
            "DOCKER_HOST": "tcp://remote.invalid:2375", "GITHUB_TOKEN": "fixture-token", "GOFLAGS": "-tags=unreviewed",
        }):
            env = proof.command_environment(Path(directory), "/public-module-cache", "/pinned-go")
        for forbidden in ("AWS_SECRET_ACCESS_KEY", "KUBECONFIG", "GITHUB_TOKEN"):
            self.assertNotIn(forbidden, env)
        self.assertEqual(env["DOCKER_HOST"], "unix:///var/run/docker.sock")
        self.assertEqual(env["GOFLAGS"], "-mod=readonly")
        self.assertEqual(env["GOTOOLCHAIN"], "local")
        self.assertEqual(env["GOPROXY"], "off")

    def test_unsupported_host_cannot_claim_identity_proof(self):
        with patch.object(proof.sys, "platform", "win32"), patch.object(proof, "run_fixture") as run:
            with self.assertRaises(RuntimeError):
                proof.main()
            run.assert_not_called()

    def test_reply_inbox_is_not_misrepresented_as_denied(self):
        # The real canonical principal is allowed _INBOX.>. It would be false
        # to invent a foreign-inbox denial that these broker grants do not make.
        self.assertTrue(all("_INBOX" not in s for s in proof.SUBSCRIPTIONS.values()))
        self.assertEqual(len(proof.PUBLICATIONS), 14)
        self.assertEqual(len(proof.SUBSCRIPTIONS), 4)

    def test_generated_ownership_grammar_and_source_run_binding(self):
        expected = {"source": "a" * 40, "tree": "b" * 40, "run": "17", "attempt": "2"}
        owner = {"schema": 1, **expected, "cluster": "feed-proof-012345abcdef"}
        self.assertEqual(proof.validate_ownership(owner, expected), owner)
        self.assertEqual(proof.owned_image(owner["cluster"]), "kanz-agent-identity-012345abcdef:fixture")
        for key, value in (("source", "c" * 40), ("tree", "c" * 40), ("run", "18"), ("attempt", "1"),
                           ("cluster", "agent-proof-012345abcdef"), ("cluster", "feed-proof-ABCDEF012345"),
                           ("cluster", "feed-proof-abc"), ("cluster", "../production"), ("schema", True)):
            mutant = {**owner, key: value}
            with self.assertRaises(ValueError, msg=key + "=" + str(value)):
                proof.validate_ownership(mutant, expected)

    def test_missing_or_foreign_owner_never_deletes_anything(self):
        class ReadOnlyRunner:
            calls = []
            def command(self, args, **kwargs):
                self.calls.append(args)
                if args[:3] == ["git", "rev-parse", "HEAD"]:
                    return "a" * 40
                if args[:3] == ["git", "rev-parse", "HEAD^{tree}"]:
                    return "b" * 40
                raise AssertionError("unowned cleanup reached a resource command")
        owner = {"schema": 1, "source": "a" * 40, "tree": "b" * 40, "run": "17", "attempt": "1", "cluster": "feed-proof-012345abcdef"}
        for value, wanted_status in ((None, 0), (owner, 1), ({**owner, "attempt": "2", "cluster": "production"}, 1)):
            runner = ReadOnlyRunner()
            with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"GITHUB_RUN_ID": "17", "GITHUB_RUN_ATTEMPT": "2"}), \
                    patch.object(proof, "Runner", return_value=runner), patch.object(proof, "read_ownership", return_value=value), \
                    patch.object(proof, "write_json") as write:
                self.assertEqual(proof.cleanup_recorded(Path(directory)), wanted_status)
                receipt_value = write.call_args.args[2]
                self.assertFalse(receipt_value["recorded"])
                self.assertNotIn("cluster_absent", receipt_value)
                self.assertTrue(all(call[0] == "git" for call in runner.calls))

    def test_cleanup_deletes_only_exact_recorded_cluster(self):
        name = "feed-proof-012345abcdef"
        class OwnedRunner:
            def __init__(self):
                self.calls, self.deleted = [], False
            def command(self, args, **kwargs):
                self.calls.append(args)
                if args[:3] == ["kind", "get", "clusters"]:
                    return "foreign-cluster" + ("\n" + name if not self.deleted else "")
                if args[:3] == ["kind", "delete", "cluster"]:
                    self.assertion = args[3:5] == ["--name", name]
                    self.deleted = True
                    return ""
                if args[:3] in (["docker", "ps", "-aq"], ["docker", "image", "ls"]):
                    return ""
                raise AssertionError("unexpected cleanup operation")
        runner = OwnedRunner()
        result = proof.cleanup_owner(runner, {"cluster": name}, Path("private-kubeconfig"))
        self.assertTrue(all(result.values()))
        self.assertTrue(runner.assertion)
        deletions = [args for args in runner.calls if args[:2] == ["kind", "delete"]]
        self.assertEqual(deletions, [["kind", "delete", "cluster", "--name", name, "--kubeconfig", "private-kubeconfig"]])
        self.assertTrue(all("foreign-cluster" not in args for args in runner.calls))

    @unittest.skipUnless(sys.platform == "linux" and hasattr(os, "O_NOFOLLOW"), "protected filesystem proof runs on Linux")
    def test_retained_evidence_refuses_symlink_and_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            private = proof.protected_directory(str(root / "private"))
            proof.write_evidence(private, {"passed": False})
            with self.assertRaises(FileExistsError):
                proof.write_evidence(private, {"passed": True})
            (root / "link").symlink_to(private, target_is_directory=True)
            with self.assertRaises(ValueError):
                proof.protected_directory(str(root / "link"))


if __name__ == "__main__":
    unittest.main()
