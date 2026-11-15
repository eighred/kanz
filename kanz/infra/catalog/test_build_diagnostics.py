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
        # This same pre-authentication host control exercises runtime projection
        # even when the real fixture stops before its target-side unit suite.
        runtime = ("unable to load config: permission denied " + secret + "\n").encode()
        projected = qualify.runtime_output(runtime, 1, "broker-startup-log")
        self.assertEqual(projected["category"], "multiple-signals")
        self.assertEqual(projected["matchedSignals"], ["configuration-refused", "permission-refused"])
        self.assertEqual(projected["capturedSHA256"], hashlib.sha256(runtime).hexdigest())
        self.assertEqual(projected["capturedBytes"], len(runtime))
        self.assertTrue(projected["informationalOnly"])
        self.assertNotIn(secret, json.dumps(projected))
        # A PathError-shaped sequence binds only its adjacent public resource
        # class. Tokens on other lines or secret path/level/operation values
        # never become retained text or an attributed vendor failure.
        contextual = ("WARN open /tmp/redpanda.yaml: permission denied " + secret + "\n"
                      "write /var/lib/redpanda/data/cache: read-only file system " + secret + "\n"
                      "open /proc/sys/vm/swappiness: operation not permitted " + secret + "\n"
                      "open /tmp/redpanda.yaml-" + secret + ": permission denied\n"
                      + secret + " /tmp/redpanda.yaml: permission denied\n"
                      "open /tmp/redpanda.yaml\npermission denied " + secret + "\n"
                      "open /tmp/redpanda.yaml: permission denied; write /var/lib/redpanda/data: operation not permitted\n").encode()
        contexts = qualify.runtime_output(contextual, 0, "broker-startup-log")["lineContext"]
        records = contexts["records"]
        config = next(record for record in records if record["resource"] == "selected-broker-config")
        self.assertEqual((config["operation"], config["error"], config["binding"]), ("open", "permission-denied", "path-error-sequence"))
        self.assertEqual((config["stage"], config["level"]), ("unclassified", "unclassified"))
        self.assertTrue(any(record["resource"] == "selected-broker-data" and record["operation"] == "write" for record in records))
        self.assertTrue(any(record["resource"] == "proc-sys-vm" and record["error"] == "operation-not-permitted" for record in records))
        self.assertTrue(any(record["binding"] == "multiple-path-error-sequences" and record["resource"] == "multiple" and record["error"] == "multiple" for record in records))
        self.assertEqual(sum(record["count"] for record in records if record["binding"] == "unclassified"), 2)
        self.assertTrue(any(record["operation"] == "open" and record["resource"] == "unclassified" for record in records))
        self.assertFalse(contexts["truncated"])
        for public_path, classification in (("/tmp", "bounded-temporary-root"), ("/etc/redpanda/redpanda.yaml", "packaged-vendor-config"), ("/var/lib/redpanda/coredump/core", "packaged-coredump-directory")):
            packaged = qualify.runtime_output(("open " + public_path + ": permission denied " + secret).encode(), 1, "broker-startup-log")["lineContext"]["records"][0]
            self.assertEqual(packaged["resource"], classification)
            self.assertNotIn(secret, json.dumps(packaged))
        for private_path in ("/tmp/" + secret, "/tmp/REDPANDA.yaml", "/tmp/redpanda.yaml/" + secret, "/etc/redpanda/redpanda.yaml-" + secret, "/var/lib/redpanda/data-" + secret, "/var/lib/redpanda/data/../" + secret, "/var/lib/redpanda/coredump-" + secret, "/proc-" + secret, "/sys-" + secret):
            negative = qualify.runtime_output((secret + " open " + private_path + ": permission denied").encode(), 1, "broker-startup-log")["lineContext"]["records"][0]
            self.assertEqual((negative["resource"], negative["level"], negative["stage"]), ("unclassified",) * 3)
            self.assertNotIn(secret, json.dumps(negative))
        self.assertNotIn(secret, json.dumps(contexts))
        for raw_path in ("/tmp", "/var/lib/redpanda", "/proc/sys"):
            self.assertNotIn(raw_path, json.dumps(contexts))
        resources = ("/tmp/redpanda.yaml", "/var/lib/redpanda/data", "/proc/sys/kernel", "/proc/sys/vm", "/proc/sys/fs", "/proc/1/status", "/sys/kernel", "/unknown/" + secret)
        lines = [operation + " " + path + ": permission denied" for operation in ("open", "write") for path in resources]
        lines += ["mkdir /tmp/redpanda.yaml: permission denied", lines[0]]
        bounded_contexts = qualify.runtime_output("\n".join(lines).encode(), 1, "broker-startup-log")["lineContext"]
        self.assertEqual((bounded_contexts["recordLimit"], len(bounded_contexts["records"]), bounded_contexts["signalLines"], bounded_contexts["omittedSignalLines"]), (16, 16, 18, 1))
        self.assertEqual(bounded_contexts["records"][0]["count"], 2)
        self.assertTrue(bounded_contexts["truncated"])
        self.assertEqual(sum(record["count"] for record in bounded_contexts["records"]) + bounded_contexts["omittedSignalLines"], bounded_contexts["signalLines"])
        tail = qualify.runtime_output(("\n".join([lines[0]] * 41)).encode(), 1, "broker-startup-log")["lineContext"]
        self.assertEqual((tail["lineLimit"], tail["inspectedLines"], tail["omittedEarlierLines"], tail["records"][0]["count"]), (40, 40, 1, 40))
        self.assertTrue(tail["truncated"])
        self.assertNotIn(secret, json.dumps(bounded_contexts))
        # Compiled C++ wrappers and API literals do not establish a PathError
        # grammar. Keep errno text and same-line co-occurrence independent of
        # the still-unclassified attributed operation/resource/error fields.
        cpp = ('Failure during startup: filesystem error: chmod failed: Operation not permitted ["/tmp/redpanda.yaml"] ' + secret).encode()
        cpp_projection = qualify.runtime_output(cpp, 0, "broker-startup-log")
        cpp_record = cpp_projection["lineContext"]["records"][0]
        self.assertEqual(cpp_projection["matchedSignals"], ["chmod-failed", "filesystem-error-wrapper", "permission-refused", "startup-failure-wrapper"])
        self.assertEqual([cpp_record[key] for key in ("binding", "operation", "resource", "error", "stage", "level")], ["unclassified"] * 6)
        self.assertEqual(cpp_record["errnoSignals"], ["operation-not-permitted"])
        self.assertEqual(cpp_record["coOccurrence"], {"resourceSignals": ["selected-broker-config"], "apiSignals": ["chmod-failed"], "templateSignals": ["filesystem-error-wrapper", "startup-failure-wrapper"]})
        for component, signal_class in (("stall detector", "perf-event-stall-fallback"), ("cpu profiler", "perf-event-cpu-profiler-fallback")):
            fallback = ("Creation of perf_event based " + component + " failed: falling back to posix timer: perf_event_open() failed: Operation not permitted " + secret).encode()
            observation = qualify.runtime_output(fallback, 1, "broker-startup-log")
            row = observation["lineContext"]["records"][0]
            self.assertIn(signal_class, observation["matchedSignals"])
            self.assertNotIn("startup-failure-wrapper", observation["matchedSignals"])
            self.assertEqual(row["coOccurrence"], {"resourceSignals": [], "apiSignals": ["perf-event-open-failed"], "templateSignals": [signal_class]})
            self.assertEqual((row["errnoSignals"], row["binding"], row["level"]), (["operation-not-permitted"], "unclassified", "unclassified"))
            self.assertNotIn(secret, json.dumps(observation))
        wrapper = qualify.runtime_output(("Failure during startup: " + secret).encode(), 1, "broker-startup-log")
        self.assertEqual(wrapper["matchedSignals"], ["startup-failure-wrapper"])
        self.assertEqual(wrapper["lineContext"]["signalLines"], 1)
        self.assertEqual(wrapper["lineContext"]["records"][0]["errnoSignals"], [])
        split = qualify.runtime_output(('filesystem error: chmod failed ' + secret + '\nOperation not permitted\n["/tmp/redpanda.yaml"]').encode(), 1, "broker-startup-log")["lineContext"]["records"]
        self.assertEqual(len(split), 2)
        self.assertEqual(split[0]["errnoSignals"], [])
        self.assertEqual(split[1]["errnoSignals"], ["operation-not-permitted"])
        self.assertEqual(split[1]["coOccurrence"], {"resourceSignals": [], "apiSignals": [], "templateSignals": []})
        mixed = qualify.runtime_output(("Failure during startup: Creation of perf_event based stall detector failed: falling back to posix timer: " + secret).encode(), 1, "broker-startup-log")["lineContext"]["records"][0]
        self.assertEqual(mixed["coOccurrence"]["templateSignals"], ["perf-event-stall-fallback", "startup-failure-wrapper"])
        self.assertEqual((mixed["stage"], mixed["level"], mixed["binding"]), ("unclassified",) * 3)
        for text, template in (("Not enough space", "storage-space-message"),
                               ("no disk space; bytes free less than configurable threshold", "storage-free-threshold-message"),
                               ("Memory: '" + secret + "' below required: '" + secret + "'", "memory-below-required-message"),
                               (secret + " - Error reading diskstats: " + secret, "diskstats-read-message"),
                               ("I/O error during read!  Disk failure?", "disk-read-message"),
                               ("Failed to read pid file " + secret + ":" + secret, "pid-read-message"),
                               ("Failed to unlink pid file " + secret + ":" + secret, "pid-unlink-message")):
            projected_template = qualify.runtime_output((text + " " + secret).encode(), 1, "broker-startup-log")
            row = projected_template["lineContext"]["records"][0]
            self.assertEqual(row["coOccurrence"]["templateSignals"], [template])
            self.assertEqual([row[key] for key in ("binding", "operation", "resource", "error", "stage", "level")], ["unclassified"] * 6)
            self.assertNotIn(secret, json.dumps(projected_template))
        configuration = qualify.runtime_output(b"Threshold of minimum bytes free space before setting storage space alert.", 0, "broker-startup-log")
        self.assertEqual(configuration["matchedSignals"], [])
        self.assertEqual(configuration["lineContext"]["records"], [])
        for broken_memory in ("Memory: '" + secret + "'\nbelow required: '" + secret + "'",
                              "Memory: '" + secret + "' below required:\n'" + secret + "'",
                              "Memory: '" + secret + "\n' below required: '" + secret + "'",
                              "Memory: '" + secret + "'\rbelow required: '" + secret + "'"):
            negative_memory = qualify.runtime_output(broken_memory.encode(), 1, "broker-startup-log")
            self.assertEqual(negative_memory["matchedSignals"], [])
            self.assertEqual(negative_memory["lineContext"]["records"], [])
            self.assertNotIn(secret, json.dumps(negative_memory))
        for body in ('open failed: Permission denied ["/tmp/redpanda.yaml$' + secret + '"]', 'open failed: Permission denied ["/tmp/redpanda.yaml ' + secret + '"]', 'chmod failed-' + secret + ': Operation not permitted', 'Failure during startup-' + secret + ': read-only file system'):
            unbound = qualify.runtime_output(body.encode(), 1, "broker-startup-log")["lineContext"]["records"][0]
            self.assertEqual([unbound[key] for key in ("operation", "resource", "error", "binding", "stage", "level")], ["unclassified"] * 6)
            self.assertEqual(unbound["coOccurrence"]["resourceSignals"], [])
            self.assertNotIn(secret, json.dumps(unbound))
        self.assertNotIn(secret, json.dumps((cpp_projection, wrapper, split, mixed)))
        for raw in ("/tmp/redpanda.yaml", "filesystem error:", "chmod failed:", "Failure during startup:"):
            self.assertNotIn(raw, json.dumps(cpp_projection))
        for body, code, source in ((runtime, secret, "broker-startup-log"), (runtime, 1, secret), (b"x" * (qualify.READINESS_OUTPUT_LIMIT + 1), 1, "broker-startup-log")):
            try:
                qualify.runtime_output(body, code, source)
            except ValueError as error:
                refused = str(error) + traceback.format_exc()
            else:
                self.fail("unknown/oversized runtime projection must refuse")
            self.assertNotIn(secret, refused)



if __name__ == "__main__":
    unittest.main()
