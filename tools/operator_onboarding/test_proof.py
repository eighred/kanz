import copy
import json
import unittest

from tools.operator_onboarding.proof import browser_error, image_events, unauthorized_pull


IMAGE = "ghcr.io/eighred/web-bff@sha256:" + "1" * 64
POD = {"metadata": {"uid": "worker-pod"}, "status": {"containerStatuses": [
    {"name": "web-bff", "image": IMAGE, "state": {"waiting": {"reason": "ErrImagePull"}}}]}}
EVENT = {"involvedObject": {"uid": "worker-pod", "fieldPath": "spec.containers{web-bff}"},
         "source": {"component": "kubelet"}, "reason": "Failed",
         "message": f'Failed to pull image "{IMAGE}": failed to fetch anonymous token: 403 Forbidden'}


class PullEvidenceTest(unittest.TestCase):
    def test_browser_failure_keeps_only_known_stage_and_numeric_status(self):
        report = {"stage": "node submission", "category": "timeout", "statuses": {
            "addNode": 403, "secret": 200, "session": "synthetic-private-value"}}
        result = browser_error("untrusted subprocess output\n" + json.dumps(report))
        self.assertEqual(result, 'browser timeout at node submission; HTTP statuses {"addNode": 403}')
        report["stage"] = "synthetic-private-value"
        self.assertNotIn("synthetic-private-value", browser_error(json.dumps(report)))

    def test_missing_credential_requires_matching_workload_and_registry_refusal(self):
        self.assertTrue(unauthorized_pull(POD, [EVENT], IMAGE))
        other = IMAGE.replace("web-bff", "another-service")
        for changed in ({"reason": "FailedMount"}, {"source": {"component": "scheduler"}},
                        {"involvedObject": {"uid": "other", "fieldPath": "spec.containers{web-bff}"}},
                        {"involvedObject": {"uid": "worker-pod", "fieldPath": "spec.containers{sidecar}"}},
                        {"message": EVENT["message"].replace(IMAGE, other)},
                        {"message": f'Failed to pull image "{IMAGE}": context deadline exceeded'},
                        {"message": "permission denied while mounting the volume"}):
            with self.subTest(changed=changed):
                self.assertFalse(unauthorized_pull(POD, [dict(EVENT, **changed)], IMAGE))
        for changed in ({"image": other}, {"name": "sidecar"}, {"state": {"running": {}}}):
            pod = copy.deepcopy(POD)
            pod["status"]["containerStatuses"][0].update(changed)
            self.assertFalse(unauthorized_pull(pod, [EVENT], IMAGE))

    def test_positive_receipt_excludes_the_already_cached_event(self):
        event = dict(EVENT, reason="Pulled", message=f'Successfully pulled image "{IMAGE}" in 2s')
        self.assertTrue(list(image_events(POD, [event], "Pulled", IMAGE, "Successfully pulled image")))
        event["message"] = f'Container image "{IMAGE}" already present on machine'
        self.assertFalse(list(image_events(POD, [event], "Pulled", IMAGE, "Successfully pulled image")))
