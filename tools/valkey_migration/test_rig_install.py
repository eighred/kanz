import copy
import json
from pathlib import Path
from unittest import TestCase
from unittest.mock import Mock

import yaml

import rig_install


class RigCacheSelectionTests(TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parents[2]
        self.resources = list(yaml.safe_load_all((root / "kanz/infra/messaging/redis.yaml").read_text(encoding="utf-8")))
        self.original, = [doc for doc in self.resources if doc["kind"] == "StatefulSet"]
        self.existing = copy.deepcopy(self.original)
        self.existing["metadata"].update(uid="set-uid", resourceVersion="17",
                                         annotations={rig_install.ORIGINAL_CLAIM: "claim-uid"})
        self.claim = {"metadata": {"uid": "claim-uid"}}

    def test_fresh_install_reserves_claim_then_creates_set_without_upsert(self):
        k = Mock(side_effect=["", "", "claim-uid", "", "", ""])
        rig_install.install(k, self.resources)
        ops = k.call_args_list
        self.assertEqual([call.args[:2] for call in ops[:2]],
                         [("get", "statefulset/redis"), ("get", "pvc/data-redis-0")])
        self.assertEqual([call.args[0] for call in ops[2:]], ["create", "create", "apply", "apply"])
        claim, sts = [json.loads(call.kwargs["data"]) for call in ops[2:4]]
        self.assertEqual(claim["metadata"], {"name": "data-redis-0", "namespace": "kanz-messaging"})
        self.assertEqual(claim["spec"], sts["spec"]["volumeClaimTemplates"][0]["spec"])
        self.assertEqual(sts["metadata"]["annotations"][rig_install.ORIGINAL_CLAIM], "claim-uid")

    def test_reapply_preserves_original_claim_and_conditions_write_on_observed_set(self):
        reads = [json.dumps(self.existing), json.dumps(self.claim)]
        preflight = Mock(side_effect=reads)
        rig_install.install(preflight, self.resources, preflight=True)
        self.assertEqual(preflight.call_count, 2)
        k = Mock(side_effect=[*reads, "", "", ""])
        rig_install.install(k, self.resources)
        write = k.call_args_list[2]
        self.assertEqual(write.args[:2], ("apply", "--server-side"))
        self.assertEqual(json.loads(write.kwargs["data"])["metadata"], self.existing["metadata"])

    def test_incomplete_unknown_or_changed_state_never_writes(self):
        for case in ("orphan", "missing", "legacy", "unknown", "untagged", "replaced", "terminating", "different-ordinal"):
            with self.subTest(case=case):
                current = copy.deepcopy(self.existing)
                claim = copy.deepcopy(self.claim)
                if case == "orphan":
                    current = None
                elif case == "missing":
                    claim = None
                elif case in ("legacy", "unknown"):
                    current["spec"]["template"]["spec"]["containers"][0]["image"] = (
                        "redis:7.4.5" if case == "legacy" else "valkey/valkey:latest")
                elif case == "untagged":
                    current["metadata"].pop("annotations")
                elif case == "replaced":
                    claim["metadata"]["uid"] = "replacement"
                elif case == "terminating":
                    claim["metadata"]["deletionTimestamp"] = "2026-11-18T12:00:00Z"
                elif case == "different-ordinal":
                    current["spec"]["ordinals"] = {"start": 1}
                k = Mock(side_effect=[json.dumps(current) if current else "", json.dumps(claim) if claim else ""])
                with self.assertRaises(rig_install.Refused):
                    rig_install.install(k, self.resources)
                self.assertEqual([call.args[0] for call in k.call_args_list], ["get", "get"])

    def test_api_error_or_concurrent_creation_never_reaches_statefulset_upsert(self):
        for prior in ([], [""], ["", ""], ["", "", "claim-uid"]):
            with self.subTest(prior=prior):
                k = Mock(side_effect=[*prior, rig_install.Refused("API error")])
                with self.assertRaises(rig_install.Refused):
                    rig_install.install(k, self.resources)
                self.assertNotIn("apply", [call.args[0] for call in k.call_args_list])
