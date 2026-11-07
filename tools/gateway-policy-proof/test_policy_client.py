import copy
import json
import unittest
from unittest.mock import Mock

import transaction as tx
from policy_client import KubernetesClient
from test_transaction import policy


class PolicyClientTests(unittest.TestCase):
    def test_raw_paths_preserve_metadata_and_server_dry_run(self):
        for replacing in (False, True):
            for dry in (False, True):
                obj = policy(tx.NAMES[0])
                obj["metadata"]["annotations"] = {"kubectl.kubernetes.io/last-applied-configuration": '{"old":true}', "fixture.example/custom": "keep"}
                if replacing:
                    obj["metadata"].update(uid="a"*8+"-"+"a"*4+"-"+"a"*4+"-"+"a"*4+"-"+"a"*12, resourceVersion="12")
                before = copy.deepcopy(obj)
                returned = tx.semantic(obj)
                api = Mock(return_value=json.dumps(returned))
                KubernetesClient(api, "cluster").write(obj, replacing, dry)
                path = "/apis/networking.k8s.io/v1/namespaces/kanz-services/networkpolicies"
                if replacing: path += "/" + tx.NAMES[0]
                if dry: path += "?dryRun=All"
                api.assert_called_once_with("replace" if replacing else "create", "--raw", path, "-f", "-", value=before)
                self.assertEqual(obj, before)

    def test_actual_fixture_preserves_prior_cases_and_binds_metadata_controls(self):
        import inspect
        import transaction_fixture as fixture
        original = ["bounded_xz_transport"] + ["response_loss_" + str(i) for i in range(1, 9)] + [
            "dry_run_no_write", "four_policy_success", "concurrent_change_refuses_rollback", "empty_endpoint_slice_null",
            "ipv6_endpoint_metadata", "deployment_uid_lineage", "controller_adoption_refusal", "never_ready_endpoint_nonparticipation",
            "publish_unready_refusal", "actual_tcp_policy_controls", "probe_uid_cleanup"]
        self.assertEqual([v for v in fixture.CASES[:21] if v != "annotation_transport_preserved"], original)
        self.assertEqual(fixture.CASES.index("annotation_transport_preserved"), fixture.CASES.index("four_policy_success") + 1)
        source = inspect.getsource(fixture.run_fixture)
        markers = ['report["annotationTransport"] = annotation_transport_control(k, client)',
                   'for at in range(1, 9):', 'report["annotationTransport"]["rollbackPreserved"] = True',
                   'report["cases"].append("four_policy_success")',
                   'report["annotationTransport"]["finalizePreserved"] = True',
                   'validate_annotation_control(report["annotationTransport"])',
                   'report["cases"].append("annotation_transport_preserved")']
        def wired(text):
            return all(text.count(v) == 1 for v in markers) and [text.index(v) for v in markers] == sorted(text.index(v) for v in markers)
        self.assertTrue(wired(source))
        for marker in markers:
            with self.subTest(marker=marker): self.assertFalse(wired(source.replace(marker, "removed")))

    def test_invalid_identity_refuses_before_transport(self):
        original = policy(tx.NAMES[0])
        mutations = [("kind", "Pod"), ("apiVersion", "v1"), ("namespace", "other"),
                     ("name", "allow-gateway-to-accounting?dryRun=All"), ("name", "unowned")]
        for key, value in mutations:
            obj = copy.deepcopy(original)
            (obj["metadata"] if key in ("namespace", "name") else obj)[key] = value
            for replacing in (False, True):
                api = Mock()
                with self.subTest(key=key, value=value, replacing=replacing), self.assertRaises(tx.Refusal):
                    KubernetesClient(api, "cluster").write(obj, replacing)
                api.assert_not_called()
        for replacing, obj in ((True, original), (False, {**original, "metadata": {**original["metadata"], "uid": "foreign"}})):
            api = Mock()
            with self.assertRaises(tx.Refusal): KubernetesClient(api, "cluster").write(obj, replacing)
            api.assert_not_called()

    def test_dry_run_metadata_changes_are_not_normalized_away(self):
        obj = policy(tx.NAMES[0])
        obj["metadata"]["annotations"] = {"kubectl.kubernetes.io/last-applied-configuration": "original"}
        changed = copy.deepcopy(obj)
        changed["metadata"]["annotations"]["kubectl.kubernetes.io/last-applied-configuration"] = "rewritten"
        with self.assertRaises(tx.Refusal):
            KubernetesClient(Mock(return_value=json.dumps(changed)), "cluster").dry_run(obj, False)


if __name__ == "__main__":
    unittest.main()
