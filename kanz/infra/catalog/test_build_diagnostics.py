"""Synthetic build diagnostics: no Docker, registry profile or target execution."""
import hashlib
import importlib.util
import json
from pathlib import Path
import traceback
import unittest
from unittest import mock

import image_patch
import qualify

spec = importlib.util.spec_from_file_location("diagnostic_verify_patch", image_patch.HERE / "verify_patch.py")
verify_patch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verify_patch)


class BuildDiagnosticControls(unittest.TestCase):
    def test_failed_build_diagnostics_keep_only_closed_public_information(self):
        secret = "one-run-registry-credential-marker"
        selection = image_patch.lock()
        snapshot = {"packages": {"linux-libc-dev:amd64": selection["before"]}, "python": {"acryl-datahub": selection["cliPackageVersion"]}, "pythonVersion": "3.11.14"}
        observation = verify_patch.observation("before", snapshot, selection)
        output = ("Step 5/6 : RUN printf KANZ_CATALOG_PATCH_PHASE=completed\n"
                  "KANZ_CATALOG_PATCH_PHASE=before-inventory\n"
                  "KANZ_CATALOG_PATCH_OBSERVATION=" + json.dumps(observation) + "\n"
                  "permission denied: " + secret + "\nno space left on device\n").encode()
        diagnostic = image_patch.build_diagnostic(output, 1)
        self.assertEqual(diagnostic["category"], "build-multiple-signals")
        self.assertEqual(set(diagnostic["matchedSignals"]), {"build-permission-refused", "build-disk-budget"})
        self.assertEqual(diagnostic["lastRecipePhase"], "before-inventory")
        self.assertEqual(diagnostic["observations"], [observation])
        self.assertEqual(diagnostic["capturedBytes"], len(output))
        self.assertEqual(diagnostic["capturedSHA256"], hashlib.sha256(output).hexdigest())
        self.assertTrue(diagnostic["informationalOnly"])
        self.assertNotIn(secret, json.dumps(diagnostic))
        for changed in (dict(observation, pythonVersion=secret), dict(observation, private=secret), dict(observation, packageCount=secret)):
            refused = image_patch.build_diagnostic(("KANZ_CATALOG_PATCH_OBSERVATION=" + json.dumps(changed)).encode(), 1)
            self.assertTrue(refused["observationRefused"])
            self.assertEqual(refused["observations"], [])
            self.assertNotIn(secret, json.dumps(refused))
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment, fixture.preparation_evidence = "offline-ingestion-build", {}, {"build": {}}
        process = mock.Mock()
        process.poll.return_value = 1
        with mock.patch.object(qualify.subprocess, "Popen", return_value=process), mock.patch.object(qualify, "capture_process", return_value=(1, output)):
            try:
                fixture.run(["docker", "build", "--build-arg", "PRIVATE=" + secret], build=True)
            except ValueError as error:
                exception = str(error) + traceback.format_exc()
            else:
                self.fail("failed build must refuse")
        self.assertNotIn(secret, exception + json.dumps(fixture.preparation_evidence))
        self.assertEqual(fixture.preparation_evidence["build"]["diagnostic"], diagnostic)
        with self.assertRaises(ValueError):
            fixture.run(["unrelated-command"], build=True)
        completed = image_patch.build_diagnostic(b"KANZ_CATALOG_PATCH_PHASE=completed\nKANZ_CATALOG_PATCH_PROOF=passed\n", 0)
        self.assertTrue(completed["informationalOnly"] and completed["packageProofObserved"])
        owner = {"nonce": "0123456789abcdef", "network": "kanz-catalog-0123456789abcdef", "sourceSHA": "a" * 40, "runID": "123", "runAttempt": "1"}
        record = image_patch.owned_spec(owner, Path("/owned/run/owner.json"))
        selected = image_patch.image_identity.selections()["ingestion"]
        base = {"Id": selected["configDigest"], "Os": "linux", "Architecture": "amd64", "Config": {}, "RootFS": {"Type": "layers", "Layers": selected["rootFSDiffIDs"]}, "RepoDigests": [selected["executionRepository"] + "@" + selected["manifestDigest"]]}
        base_identity = image_patch.validate_base(base, record)
        def execute(args, **_kwargs):
            if args[:3] == ["docker", "image", "inspect"]:
                return json.dumps([base])
            if args[:3] in (["docker", "image", "ls"], ["docker", "ps", "--all"]):
                return ""
            raise AssertionError("diagnostic markers cannot authorize mutation")
        with mock.patch.object(image_patch, "read_marker", side_effect=lambda _record, phase: image_patch.marker_body(record, phase, base=base_identity) if phase == "started" else None), mock.patch.object(image_patch.time, "sleep"), self.assertRaisesRegex(ValueError, "completion remains unknown"):
            image_patch.cleanup(record, execute)



if __name__ == "__main__":
    unittest.main()
