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
        fingerprint = wrapper["lineContext"]["records"][0]["startupMessage"]
        self.assertEqual((fingerprint["status"], fingerprint["category"], fingerprint["prefixCount"], fingerprint["bodyBytes"], fingerprint["bodySHA256"]), ("hashed", "bounded-body", 1, len(secret.encode()), hashlib.sha256(secret.encode()).hexdigest()))
        self.assertEqual((fingerprint["framing"], fingerprint["trim"], fingerprint["bodyLimitBytes"]), ("single-logical-line", "ascii-whitespace", 4096))
        framed = qualify.runtime_output(("\tFailure during startup: \t " + secret + " \t").encode(), 1, "broker-startup-log")["lineContext"]["records"][0]["startupMessage"]
        self.assertEqual(framed, fingerprint)
        non_ascii_space = (secret + "\u00a0").encode()
        exact_bytes = qualify.runtime_output(b"Failure during startup: " + non_ascii_space, 1, "broker-startup-log")["lineContext"]["records"][0]["startupMessage"]
        self.assertEqual((exact_bytes["bodyBytes"], exact_bytes["bodySHA256"]), (len(non_ascii_space), hashlib.sha256(non_ascii_space).hexdigest()))
        maximum = qualify.runtime_output(b"Failure during startup: " + b"x" * 4096, 1, "broker-startup-log")["lineContext"]["records"][0]["startupMessage"]
        self.assertEqual((maximum["status"], maximum["bodyBytes"], maximum["bodySHA256"]), ("hashed", 4096, hashlib.sha256(b"x" * 4096).hexdigest()))
        for body, category, count in ((b"Failure during startup:\t" + secret.encode(), "unknown-prefix", 0),
                                      (b"failure during startup: " + secret.encode(), "unknown-prefix", 0),
                                      (b"Failure during startup: Failure during startup: " + secret.encode(), "multiple-prefixes", 2),
                                      (b"Failure during startup: \t ", "empty-body", 1),
                                      (b"Failure during startup: " + b"x" * 4097, "body-budget", 1),
                                      (b"Failure during startup: " + secret.encode() + b"\xff", "invalid-utf8", 1)):
            refused_metadata = qualify.runtime_output(body, 1, "broker-startup-log")["lineContext"]["records"][0]["startupMessage"]
            self.assertEqual((refused_metadata["status"], refused_metadata["category"], refused_metadata["prefixCount"]), ("refused", category, count))
            self.assertIsNone(refused_metadata["bodyBytes"])
            self.assertIsNone(refused_metadata["bodySHA256"])
            self.assertNotIn(secret, json.dumps(refused_metadata))
        non_broker = qualify.runtime_output(("Failure during startup: " + secret).encode(), 1, "readiness-command")
        self.assertIsNone(non_broker["lineContext"]["records"][0]["startupMessage"])
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            split_wrapper = qualify.runtime_output(("Failure during" + separator + "startup: " + secret).encode(), 1, "broker-startup-log")
            self.assertNotIn("startup-failure-wrapper", split_wrapper["matchedSignals"])
            self.assertEqual(split_wrapper["lineContext"]["records"], [])
            split_body = qualify.runtime_output(("Failure during startup: " + separator + secret).encode(), 1, "broker-startup-log")
            refused_metadata = split_body["lineContext"]["records"][0]["startupMessage"]
            self.assertEqual(refused_metadata["category"], "empty-body")
            self.assertIsNone(refused_metadata["bodyBytes"])
            self.assertIsNone(refused_metadata["bodySHA256"])
        separate = qualify.runtime_output(("Failure during startup: " + secret + "-one\nFailure during startup: " + secret + "-two\nFailure during startup: " + secret + "-one").encode(), 1, "broker-startup-log")["lineContext"]
        self.assertEqual((len(separate["records"]), [row["count"] for row in separate["records"]]), (2, [2, 1]))
        self.assertEqual([row["startupMessage"]["bodySHA256"] for row in separate["records"]], [hashlib.sha256((secret + suffix).encode()).hexdigest() for suffix in ("-one", "-two")])
        self.assertNotIn(secret, json.dumps((wrapper, framed, exact_bytes, non_broker, separate)))
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
                              "Memory: '" + secret + "'\rbelow required: '" + secret + "'",
                              *("Memory: '" + secret + separator + "' below required: '" + secret + "'" for separator in ("\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029")),
                              *("Memory: '" + secret + "' below required: '" + secret + separator + "'" for separator in ("\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"))):
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
        search_body = ('Likely root cause: java.nio.file.AccessDeniedException: "/usr/share/opensearch/config/opensearch.keystore.tmp" ' + secret).encode()
        search = qualify.runtime_output(search_body, 1, "search-startup-log")
        row = search["lineContext"]["records"][0]
        self.assertEqual(search["matchedSignals"], ["search-access-denied-exception", "search-root-cause-wrapper"])
        self.assertEqual(row["coOccurrence"], {"resourceSignals": ["packaged-search-config"], "apiSignals": ["search-access-denied-exception"], "templateSignals": ["search-root-cause-wrapper"]})
        self.assertEqual([row[key] for key in ("operation", "resource", "error", "stage", "level", "binding")], ["unclassified"] * 6)
        self.assertIsNone(row["startupMessage"])
        for text, expected in (("node validation exception", "search-node-validation-wrapper"),
                               ("uncaught exception in thread", "search-uncaught-wrapper"),
                               ("can not run opensearch as root", "search-root-user-message"),
                               ("failed to obtain node locks", "search-node-lock-message"),
                               ("failed to test writes in data directory", "search-data-write-message"),
                               ("unable to create temporary keystore at", "search-keystore-write-message"),
                               ("Failed to read keystore password on console", "search-keystore-console-message"),
                               ("OPENSEARCH_PATH_CONF must be set to the configuration path", "search-config-path-message"),
                               ("encountered improperly formatted JVM option in", "search-jvm-option-message"),
                               ("bootstrap checks failed", "search-bootstrap-check-message"),
                               ("org.opensearch.bootstrap.BootstrapException", "search-bootstrap-exception"),
                               ("org.opensearch.node.NodeValidationException", "search-node-validation-exception"),
                               ("java.lang.IllegalStateException", "search-illegal-state-exception"),
                               ("starting java failed with [1]", "search-java-start-message"),
                               ("Error occurred during initialization of VM", "search-jvm-initialization-message"),
                               ("Could not create the Java Virtual Machine.", "search-jvm-create-message"),
                               ("Error opening log file", "search-jvm-log-open-message"),
                               ("Invalid -Xlog option", "search-jvm-xlog-message"),
                               ("Unrecognized VM option", "search-jvm-unrecognized-option-message"),
                               ("Invalid initial heap size:", "search-jvm-initial-heap-message"),
                               ("Invalid maximum heap size:", "search-jvm-maximum-heap-message"),
                               ("Failed to start thread", "search-jvm-thread-start-message"),
                               ("Cannot create VM thread. Out of system resources.", "search-jvm-thread-resource-message"),
                               ("java.lang.RuntimeException", "search-runtime-exception"),
                               ("java.lang.NullPointerException", "search-null-pointer-exception"),
                               ("java.lang.OutOfMemoryError", "search-out-of-memory-error"),
                               ("org.opensearch.tools.launchers.JvmErgonomics.flagsFinal", "search-jvm-ergonomics-frame"),
                               ("org.opensearch.tools.launchers.JvmOptionsParser.main", "search-jvm-options-parser-frame"),
                               ("pthread_create failed", "search-pthread-create-message"),
                               ("jdk.internal.platform.CgroupSubsystemFactory.create", "search-jdk-cgroup-factory-frame"),
                               ("jdk.internal.platform.cgroupv2.CgroupV2Subsystem.getInstance", "search-jdk-cgroup-v2-frame"),
                               ("com.sun.management.internal.OperatingSystemImpl.<init>", "search-jdk-os-metrics-frame")):
            observation = qualify.runtime_output((text + " " + secret).encode(), 1, "search-startup-log")
            self.assertEqual(observation["matchedSignals"], [expected])
            self.assertNotIn(secret, json.dumps(observation))
        launcher_body = ("java.lang.RuntimeException: starting java failed with [1]\noutput:\n" + secret + "\nerror:\nError occurred during initialization of VM\nError opening log file '" + secret + "': " + secret).encode()
        launcher = qualify.runtime_output(launcher_body, 1, "search-startup-log")
        self.assertEqual(launcher["matchedSignals"], ["search-java-start-message", "search-jvm-initialization-message", "search-jvm-log-open-message", "search-runtime-exception"])
        self.assertEqual(len(launcher["lineContext"]["records"]), 3)
        self.assertEqual(launcher["lineContext"]["records"][0]["coOccurrence"]["apiSignals"], ["search-runtime-exception"])
        for row in launcher["lineContext"]["records"]:
            self.assertIsNone(row["startupMessage"])
            self.assertEqual([row[key] for key in ("operation", "resource", "error", "stage", "level", "binding")], ["unclassified"] * 6)
        self.assertNotIn(secret, json.dumps(launcher))
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            for broken in ("starting java failed with [" + separator + "1]", "starting java failed" + separator + "with [1]", "starting java failed with [1" + separator + "]"):
                negative = qualify.runtime_output((broken + " " + secret).encode(), 1, "search-startup-log")
                self.assertEqual(negative["matchedSignals"], [])
                self.assertEqual(negative["lineContext"]["records"], [])
                self.assertNotIn(secret, json.dumps(negative))
        for unknown in ("starting java failed with [0]", "starting java failed with [" + secret + "]", "starting java failed with [12345678901]", "starting java failed with [1]" + secret, "java.lang.RuntimeException" + secret, "java.lang.NullPointerException" + secret, "java.lang.OutOfMemoryError" + secret, "org.opensearch.tools.launchers.JvmErgonomics" + secret):
            self.assertEqual(qualify.runtime_output(unknown.encode(), 1, "search-startup-log")["matchedSignals"], [])
        thread = qualify.runtime_output(('Failed to start thread "' + secret + '" - pthread_create failed (' + secret + ') for attributes: ' + secret).encode(), 1, "search-startup-log")["lineContext"]["records"][0]
        self.assertEqual(thread["coOccurrence"], {"resourceSignals": [], "apiSignals": ["search-pthread-create-message"], "templateSignals": ["search-jvm-thread-start-message"]})
        self.assertEqual((thread["operation"], thread["error"], thread["stage"], thread["level"]), ("unclassified",) * 4)
        self.assertNotIn(secret, json.dumps(thread))
        for path, classification in (("/usr/share/opensearch/data/nodes/0", "selected-search-data"), ("/usr/share/opensearch/logs/opensearch.log", "selected-search-logs")):
            observed = qualify.runtime_output(('java.nio.file.AccessDeniedException: "' + path + '" ' + secret).encode(), 1, "search-startup-log")
            self.assertEqual(observed["lineContext"]["records"][0]["coOccurrence"]["resourceSignals"], [classification])
        for path in ("/usr/share/OpenSearch/config/" + secret, "/usr/share/opensearch/config-" + secret, "/usr/share/opensearch/config/../" + secret, "/unknown/" + secret):
            observed = qualify.runtime_output(('java.nio.file.AccessDeniedException: "' + path + '"').encode(), 1, "search-startup-log")
            self.assertEqual(observed["lineContext"]["records"][0]["coOccurrence"]["resourceSignals"], [])
            self.assertNotIn(secret, json.dumps(observed))
        split_search = qualify.runtime_output(('unable to create temporary keystore at ' + secret + '\n"/usr/share/opensearch/config/opensearch.keystore.tmp"').encode(), 1, "search-startup-log")
        self.assertEqual(split_search["lineContext"]["records"][0]["coOccurrence"]["resourceSignals"], [])
        for body in ("unable to create temporary\nkeystore at " + secret, "java.nio.file.\nAccessDeniedException " + secret, "java.nio.file.AccessDeniedException" + secret, "Failure during startup: " + secret):
            unknown = qualify.runtime_output(body.encode(), 1, "search-startup-log")
            self.assertEqual(unknown["matchedSignals"], [])
            self.assertEqual(unknown["lineContext"]["records"], [])
            self.assertNotIn(secret, json.dumps(unknown))
        self.assertEqual(qualify.runtime_output(search_body, 1, "broker-startup-log")["matchedSignals"], [])
        self.assertNotIn(secret, json.dumps(search))
        self.assertNotIn("/usr/share/opensearch", json.dumps(search))
        for text, expected in (("Starts OpenSearch", "search-cli-help-description"),
                               ("Option        Description", "search-cli-columns-frame"),
                               ("Positional arguments not allowed, found " + secret, "search-cli-positional-message")):
            observed = qualify.runtime_output(text.encode(), 1, "search-startup-log")
            self.assertEqual(observed["matchedSignals"], [expected])
            self.assertNotIn(secret, json.dumps(observed))
        cli = qualify.runtime_output(("ERROR: " + secret).encode(), 1, "search-startup-log")
        row = cli["lineContext"]["records"][0]
        message = row["cliErrorMessage"]
        self.assertEqual(cli["matchedSignals"], ["search-cli-error-wrapper"])
        self.assertIsNone(row["startupMessage"])
        self.assertEqual((message["status"], message["prefixCount"], message["bodyBytes"], message["bodySHA256"]), ("hashed", 1, len(secret.encode()), hashlib.sha256(secret.encode()).hexdigest()))
        self.assertEqual(qualify.runtime_output(("ERROR: \t" + secret + " \t").encode(), 1, "search-startup-log")["lineContext"]["records"][0]["cliErrorMessage"], message)
        for body, category, count in ((b"ERROR:\t" + secret.encode(), "unknown-prefix", 0),
                                      (b"ERROR:malformed ERROR: " + secret.encode(), "unknown-prefix", 1),
                                      (b"ERROR: ERROR: " + secret.encode(), "multiple-prefixes", 2),
                                      (b"ERROR: \t ", "empty-body", 1),
                                      (b"ERROR: " + b"x" * 4097, "body-budget", 1),
                                      (b"ERROR: " + secret.encode() + b"\xff", "invalid-utf8", 1)):
            refused = qualify.runtime_output(body, 1, "search-startup-log")["lineContext"]["records"][0]["cliErrorMessage"]
            self.assertEqual((refused["status"], refused["category"], refused["prefixCount"]), ("refused", category, count))
            self.assertIsNone(refused["bodyBytes"])
            self.assertIsNone(refused["bodySHA256"])
            self.assertNotIn(secret, json.dumps(refused))
        maximum = qualify.runtime_output(b"ERROR: " + b"x" * 4096, 1, "search-startup-log")["lineContext"]["records"][0]["cliErrorMessage"]
        self.assertEqual((maximum["bodyBytes"], maximum["bodySHA256"]), (4096, hashlib.sha256(b"x" * 4096).hexdigest()))
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            for broken in ("ER" + separator + "ROR: " + secret, "Starts" + separator + "OpenSearch", "Option" + separator + "Description"):
                self.assertEqual(qualify.runtime_output(broken.encode(), 1, "search-startup-log")["matchedSignals"], [])
            split_error = qualify.runtime_output(("ERROR: " + separator + secret).encode(), 1, "search-startup-log")["lineContext"]["records"][0]["cliErrorMessage"]
            self.assertEqual(split_error["category"], "empty-body")
            self.assertIsNone(split_error["bodySHA256"])
            self.assertIsNone(split_error["bodyBytes"])
        for lookalike in ("error: " + secret, " ERROR: " + secret, "INFO ERROR: " + secret, "Starts OpenSearch " + secret, "Option", "Description"):
            self.assertEqual(qualify.runtime_output(lookalike.encode(), 1, "search-startup-log")["matchedSignals"], [])
        ordinary = qualify.runtime_output(("Likely root cause: ERROR: " + secret).encode(), 1, "search-startup-log")["lineContext"]["records"][0]
        self.assertIsNone(ordinary["cliErrorMessage"])
        self.assertEqual(ordinary["signals"], ["search-root-cause-wrapper"])
        self.assertEqual(qualify.runtime_output(("ERROR: " + secret).encode(), 1, "broker-startup-log")["matchedSignals"], [])
        abnormal = qualify.runtime_output(("ERROR: OpenSearch did not exit normally - check the logs at " + secret).encode(), 1, "search-startup-log")
        self.assertEqual(abnormal["matchedSignals"], ["search-cli-abnormal-exit-message", "search-cli-error-wrapper"])
        distinct = qualify.runtime_output(("ERROR: " + secret + "-one\nERROR: " + secret + "-two\nERROR: " + secret + "-one").encode(), 1, "search-startup-log")["lineContext"]
        self.assertEqual((len(distinct["records"]), [item["count"] for item in distinct["records"]]), (2, [2, 1]))
        self.assertEqual([item["cliErrorMessage"]["bodySHA256"] for item in distinct["records"]], [hashlib.sha256((secret + suffix).encode()).hexdigest() for suffix in ("-one", "-two")])
        limited = qualify.runtime_output("\n".join("ERROR: " + secret + str(index) for index in range(17)).encode(), 1, "search-startup-log")["lineContext"]
        self.assertEqual((len(limited["records"]), limited["signalLines"], limited["omittedSignalLines"], limited["truncated"]), (16, 17, 1, True))
        self.assertNotIn(secret, json.dumps((cli, ordinary, abnormal, distinct, limited)))
        warning_body = ("WARNING: " + secret + "-one\nWARNING: " + secret + "-two").encode()
        warnings = qualify.runtime_output(warning_body, 1, "search-startup-log")
        self.assertEqual(warnings["matchedSignals"], ["search-warning-wrapper"])
        self.assertEqual((warnings["lineContext"]["signalLines"], len(warnings["lineContext"]["records"])), (2, 1))
        warning = warnings["lineContext"]["records"][0]
        self.assertEqual(warning["count"], 2)
        self.assertEqual((warning["stage"], warning["level"], warning["binding"]), ("unclassified",) * 3)
        self.assertIsNone(warning["startupMessage"])
        self.assertIsNone(warning["cliErrorMessage"])
        self.assertEqual(warnings["lineContext"]["samples"], [])
        self.assertNotIn(secret, json.dumps(warnings))
        for source in ("broker-startup-log", "readiness-command"):
            self.assertEqual(qualify.runtime_output(warning_body, 1, source)["matchedSignals"], [])
        for lookalike in ("warning: " + secret, " WARNING: " + secret, "INFO WARNING: " + secret, "WARNING:" + secret, "WARNING:\t" + secret, "WARNING:\u00a0" + secret, "\x1bWARNING: " + secret):
            self.assertEqual(qualify.runtime_output(lookalike.encode(), 1, "search-startup-log")["matchedSignals"], [])
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            self.assertEqual(qualify.runtime_output(("WARN" + separator + "ING: " + secret).encode(), 1, "search-startup-log")["matchedSignals"], [])
        warning_mixed = qualify.runtime_output("\n".join([secret + str(index) for index in range(20)] + ["WARNING: " + secret + str(index) for index in range(10)]).encode(), 1, "search-startup-log")["lineContext"]
        self.assertEqual((len(warning_mixed["records"]), warning_mixed["records"][0]["count"]), (1, 10))
        self.assertEqual([sample["lineOrdinal"] for sample in warning_mixed["samples"]], list(range(1, 9)) + list(range(14, 21)))
        self.assertEqual((warning_mixed["unknownProjection"]["eligibleLines"], warning_mixed["unknownProjection"]["sampledLines"], warning_mixed["unknownProjection"]["omittedEligibleLines"]), (20, 15, 5))
        self.assertEqual(len(warning_mixed["records"]) + len(warning_mixed["samples"]), 16)
        self.assertTrue(warning_mixed["truncated"])
        self.assertNotIn(secret, json.dumps(warning_mixed))
        forwarding_methods = ("jdk.internal.loader.NativeLibraries.load", "jdk.internal.loader.NativeLibraries$NativeLibraryImpl.open", "jdk.internal.loader.NativeLibraries.loadLibrary", "java.lang.ClassLoader.loadLibrary", "java.lang.Runtime.load0", "java.lang.System.load")
        for indent in ("", "\t"):
            forwarding_body = "\n".join(indent + "at java.base/" + method + "(" + secret + ")" for method in forwarding_methods).encode()
            forwarding = qualify.runtime_output(forwarding_body, 1, "search-startup-log")
            self.assertEqual(forwarding["matchedSignals"], ["search-native-loader-forwarding-frame"])
            self.assertEqual((len(forwarding["lineContext"]["records"]), forwarding["lineContext"]["records"][0]["count"]), (1, 6))
            record = forwarding["lineContext"]["records"][0]
            self.assertEqual((record["stage"], record["level"], record["binding"]), ("unclassified",) * 3)
            self.assertIsNone(record["startupMessage"])
            self.assertIsNone(record["cliErrorMessage"])
            self.assertEqual(forwarding["lineContext"]["samples"], [])
            self.assertNotIn(secret, json.dumps(forwarding))
            for source in ("broker-startup-log", "readiness-command"):
                self.assertEqual(qualify.runtime_output(forwarding_body, 1, source)["matchedSignals"], [])
        for prefix in (" at java.base/", "\t\tat java.base/", "INFO at java.base/", "At java.base/", "at JAVA.BASE/", "at java.base@25.0.4.1/", "at app//java.base/", "at "):
            self.assertEqual(qualify.runtime_output((prefix + forwarding_methods[0] + "(" + secret + ")").encode(), 1, "search-startup-log")["matchedSignals"], [])
        for method in (forwarding_methods[0] + "Suffix", "jdk.internal.loader.NativeLibrariesX.load", "jdk.internal.loader.NativeLibraries.find", "jdk.internal.loader.nativelibraries.load"):
            self.assertEqual(qualify.runtime_output(("at java.base/" + method + "(" + secret + ")").encode(), 1, "search-startup-log")["matchedSignals"], [])
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            broken_forwarding = "at java.base/jdk.internal.loader.NativeLibraries." + separator + "load(" + secret + ")"
            self.assertEqual(qualify.runtime_output(broken_forwarding.encode(), 1, "search-startup-log")["matchedSignals"], [])
        forwarding_mixed = qualify.runtime_output("\n".join([secret + str(index) for index in range(20)] + ["at java.base/" + method + "(" + secret + ")" for method in forwarding_methods] + ["WARNING: " + secret]).encode(), 1, "search-startup-log")["lineContext"]
        self.assertEqual((len(forwarding_mixed["records"]), [record["count"] for record in forwarding_mixed["records"]]), (2, [6, 1]))
        self.assertEqual([sample["lineOrdinal"] for sample in forwarding_mixed["samples"]], list(range(1, 8)) + list(range(14, 21)))
        self.assertEqual(len(forwarding_mixed["records"]) + len(forwarding_mixed["samples"]), 16)
        self.assertEqual(forwarding_mixed["unknownProjection"]["omittedEligibleLines"], 6)
        self.assertTrue(forwarding_mixed["truncated"])
        self.assertNotIn(secret, json.dumps(forwarding_mixed))
        unknown = qualify.runtime_output(("\t " + secret + " \t").encode(), 1, "search-startup-log")
        context = unknown["lineContext"]
        self.assertEqual(unknown["format"], {"utf8Roundtrip": True, "nulByteCount": 0, "escByteCount": 0})
        self.assertEqual(context["records"], [])
        self.assertEqual(context["samples"], [{"lineOrdinal": 1, "lineBytes": len(secret.encode()), "lineSHA256": hashlib.sha256(secret.encode()).hexdigest(), "frame": None}])
        self.assertEqual(context["unknownProjection"], {"lineLimitBytes": 4096, "ordinalBasis": "capture-one-based", "trim": "ascii-whitespace", "selection": "first-ceiling-half-last-floor-half", "consideredLines": 1, "eligibleLines": 1, "refusedLines": 0, "refusalCounts": {"invalid-utf8": 0, "empty-line": 0, "line-budget": 0}, "sampledLines": 1, "omittedEligibleLines": 0})
        self.assertFalse(context["truncated"])
        self.assertNotIn(secret, json.dumps(unknown))
        exact = qualify.runtime_output((secret + "\u00a0").encode(), 1, "search-startup-log")["lineContext"]["samples"][0]
        self.assertEqual((exact["lineBytes"], exact["lineSHA256"]), (len((secret + "\u00a0").encode()), hashlib.sha256((secret + "\u00a0").encode()).hexdigest()))
        edge = qualify.runtime_output(b"x" * 4096, 1, "search-startup-log")["lineContext"]["samples"][0]
        self.assertEqual((edge["lineBytes"], edge["lineSHA256"]), (4096, hashlib.sha256(b"x" * 4096).hexdigest()))
        multibyte = ("\u00e9" * 2048).encode()
        unicode_edge = qualify.runtime_output(b" \t" + multibyte + b" \t", 1, "search-startup-log")["lineContext"]["samples"][0]
        self.assertEqual((unicode_edge["lineBytes"], unicode_edge["lineSHA256"]), (4096, hashlib.sha256(multibyte).hexdigest()))
        for body, category in ((b"x" * 4097, "line-budget"), (("\u00e9" * 2049).encode(), "line-budget"), (b" \t ", "empty-line"), (b"\xff" + secret.encode(), "invalid-utf8")):
            refused = qualify.runtime_output(body, 1, "search-startup-log")
            self.assertEqual(refused["lineContext"]["samples"], [])
            projection = refused["lineContext"]["unknownProjection"]
            self.assertEqual((projection["consideredLines"], projection["eligibleLines"], projection["refusedLines"], projection["refusalCounts"][category]), (1, 0, 1, 1))
            self.assertTrue(refused["lineContext"]["truncated"])
            self.assertNotIn(secret, json.dumps(refused))
        for invalid_body in (secret.encode() + b"\n\xff", b"\xff\n" + secret.encode()):
            capture_invalid = qualify.runtime_output(invalid_body, 1, "search-startup-log")
            self.assertFalse(capture_invalid["format"]["utf8Roundtrip"])
            self.assertEqual(capture_invalid["lineContext"]["samples"], [])
            self.assertEqual(capture_invalid["lineContext"]["unknownProjection"]["refusalCounts"]["invalid-utf8"], 2)
            self.assertNotIn(secret, json.dumps(capture_invalid))
        empty = qualify.runtime_output(b"", 1, "search-startup-log")
        self.assertEqual(empty["lineContext"]["samples"], [])
        self.assertEqual(empty["lineContext"]["unknownProjection"]["consideredLines"], 0)
        self.assertFalse(empty["lineContext"]["truncated"])
        represented = b"\x00\x1b" + secret.encode() + b"\\x00\\x1b"
        raw_format = qualify.runtime_output(represented, 1, "search-startup-log")
        self.assertEqual(raw_format["format"], {"utf8Roundtrip": True, "nulByteCount": 1, "escByteCount": 1})
        self.assertEqual(raw_format["lineContext"]["samples"][0]["lineSHA256"], hashlib.sha256(represented).hexdigest())
        escaped_prefix = b"\x1bERROR: " + secret.encode()
        not_normalized = qualify.runtime_output(escaped_prefix, 1, "search-startup-log")
        self.assertEqual(not_normalized["matchedSignals"], [])
        self.assertEqual(not_normalized["lineContext"]["samples"][0]["lineSHA256"], hashlib.sha256(escaped_prefix).hexdigest())
        self.assertNotIn(secret, json.dumps((raw_format, not_normalized)))
        tail = qualify.runtime_output("\n".join(secret + str(index) for index in range(50)).encode(), 1, "search-startup-log")["lineContext"]
        self.assertEqual([sample["lineOrdinal"] for sample in tail["samples"]], list(range(11, 19)) + list(range(43, 51)))
        self.assertEqual((tail["omittedEarlierLines"], tail["unknownProjection"]["eligibleLines"], tail["unknownProjection"]["omittedEligibleLines"]), (10, 40, 24))
        self.assertTrue(tail["truncated"])
        known = ["Likely root cause: public", "node validation exception", "uncaught exception in thread", "can not run opensearch as root", "failed to obtain node locks", "failed to test writes in data directory", "unable to create temporary keystore at", "Failed to read keystore password on console", "OPENSEARCH_PATH_CONF must be set to the configuration path", "encountered improperly formatted JVM option in", "bootstrap checks failed", "Error occurred during initialization of VM", "Could not create the Java Virtual Machine.", "Error opening log file", "Invalid -Xlog option", "Unrecognized VM option"]
        for retained_known, expected_ordinals in ((3, list(range(1, 8)) + list(range(15, 21))), (15, [1]), (16, [])):
            mixed = qualify.runtime_output("\n".join([secret + str(index) for index in range(20)] + known[:retained_known]).encode(), 1, "search-startup-log")["lineContext"]
            self.assertEqual(len(mixed["records"]), retained_known)
            self.assertEqual([sample["lineOrdinal"] for sample in mixed["samples"]], expected_ordinals)
            self.assertEqual(len(mixed["records"]) + len(mixed["samples"]), 16)
            self.assertEqual(mixed["unknownProjection"]["omittedEligibleLines"], 20 - len(expected_ordinals))
            self.assertTrue(mixed["truncated"])
            self.assertNotIn(secret, json.dumps(mixed))
        short = qualify.runtime_output("\n".join([secret + str(index) for index in range(5)] + known[:3]).encode(), 1, "search-startup-log")["lineContext"]
        self.assertEqual([sample["lineOrdinal"] for sample in short["samples"]], [1, 2, 3, 4, 5])
        self.assertEqual(short["unknownProjection"]["omittedEligibleLines"], 0)
        self.assertFalse(short["truncated"])
        for projected in (context, capture_invalid["lineContext"], empty["lineContext"], raw_format["lineContext"], tail, mixed, short):
            counts = projected["unknownProjection"]
            self.assertEqual(counts["consideredLines"], counts["eligibleLines"] + counts["refusedLines"])
            self.assertEqual(counts["refusedLines"], sum(counts["refusalCounts"].values()))
            self.assertEqual(counts["sampledLines"], len(projected["samples"]))
            self.assertEqual(counts["eligibleLines"], counts["sampledLines"] + counts["omittedEligibleLines"])
            self.assertLessEqual(len(projected["records"]) + len(projected["samples"]), 16)
            for sample in projected["samples"]:
                self.assertEqual(set(sample), {"lineOrdinal", "lineBytes", "lineSHA256", "frame"})
                self.assertIsNone(sample["frame"])
        private_class, private_method = b"com.example.PrivateCredentialMarker", b"secretMethodMarker"
        expected_frame = {"framing": "jdk-at-frame-v1", "classBytes": len(private_class), "classSHA256": hashlib.sha256(private_class).hexdigest(), "methodBytes": len(private_method), "methodSHA256": hashlib.sha256(private_method).hexdigest()}
        frame_body = b"at " + private_class + b"." + private_method + b"(Source.java:123)"
        framed = qualify.runtime_output(b"\t " + frame_body + b" \t", 1, "search-startup-log")
        sample = framed["lineContext"]["samples"][0]
        self.assertEqual(sample, {"lineOrdinal": 1, "lineBytes": len(frame_body), "lineSHA256": hashlib.sha256(frame_body).hexdigest(), "frame": expected_frame})
        for private in (private_class.decode(), private_method.decode(), "Source.java"):
            self.assertNotIn(private, json.dumps(framed))
        for module in (b"", b"java.base/"):
            for method in (private_method, b"<init>", b"<clinit>"):
                for location in (b"Source.java:0", b"Source.java:2147483647", b"Native Method", b"Unknown Source"):
                    body = b"at " + module + private_class + b"." + method + b"(" + location + b")"
                    frame = qualify.runtime_output(body, 1, "search-startup-log")["lineContext"]["samples"][0]["frame"]
                    self.assertEqual(frame, dict(expected_frame, methodBytes=len(method), methodSHA256=hashlib.sha256(method).hexdigest()))
        maximum_class = b".".join([b"a" * 100] * 4 + [b"Z" * 108])
        maximum_method = b"m" * 128
        maximum_file = b"F" * 123 + b".java"
        maximum_frame = qualify.runtime_output(b"at " + maximum_class + b"." + maximum_method + b"(" + maximum_file + b":1)", 1, "search-startup-log")["lineContext"]["samples"][0]["frame"]
        self.assertEqual((maximum_frame["classBytes"], maximum_frame["methodBytes"]), (512, 128))
        for bounded_class in (b"a." + b"C" * 128, b"a." * 31 + b"Z"):
            bounded_frame = qualify.runtime_output(b"at " + bounded_class + b".m(Source.java:1)", 1, "search-startup-log")["lineContext"]["samples"][0]["frame"]
            self.assertEqual(bounded_frame, dict(expected_frame, classBytes=len(bounded_class), classSHA256=hashlib.sha256(bounded_class).hexdigest(), methodBytes=1, methodSHA256=hashlib.sha256(b"m").hexdigest()))
        invalid_frames = [b"At " + private_class + b".method(Source.java:1)",
                          b"at  " + private_class + b".method(Source.java:1)",
                          b"at java.logging/" + private_class + b".method(Source.java:1)",
                          b"at java.base@25.0.4.1/" + private_class + b".method(Source.java:1)",
                          b"at app//" + private_class + b".method(Source.java:1)",
                          b"at " + private_class + b".method(Source.java)",
                          b"at " + private_class + b".method(/tmp/Source.java:1)",
                          b"at " + private_class + b".method(Source.java:01)",
                          b"at " + private_class + b".method(Source.java:-1)",
                          b"at " + private_class + b".method(Source.java:2147483648)",
                          b"at " + private_class + b".method(Source.java:12345678901)",
                          b"at " + private_class + b".method(Source.java:1) trailing",
                          b"at " + private_class + b".method(Source.java:1) at other.pkg.Type.method(Other.java:2)",
                          b"at com..Private.method(Source.java:1)",
                          b"at Private.method(Source.java:1)",
                          "at com.example.Priv\u0430te.method(Source.java:1)".encode(),
                          b"at " + private_class + b".<unknown>(Source.java:1)",
                          b"at " + private_class + b".1method(Source.java:1)",
                          b"at " + private_class + b"." + b"m" * 129 + b"(Source.java:1)",
                          b"at a." + b"C" * 129 + b".m(Source.java:1)",
                          b"at " + maximum_class + b"Z.method(Source.java:1)",
                          b"at " + b"a." * 32 + b"Z.method(Source.java:1)",
                          b"at " + private_class + b".method(" + b"F" * 124 + b".java:1)",
                          b"at " + private_class + b".method(Source.java:1)\x00"]
        for body in invalid_frames:
            refused_frame = qualify.runtime_output(body, 1, "search-startup-log")["lineContext"]["samples"][0]["frame"]
            self.assertIsNone(refused_frame)
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            split_frame = qualify.runtime_output(b"at " + private_class + separator.encode() + b"." + private_method + b"(Source.java:1)", 1, "search-startup-log")
            self.assertTrue(all(sample["frame"] is None for sample in split_frame["lineContext"]["samples"]))
        bodies = [b"at " + private_class + b"." + private_method + b"(Source.java:" + str(index).encode() + b")" for index in range(20)]
        with mock.patch.object(qualify, "runtime_stackframe", wraps=qualify.runtime_stackframe) as parse:
            selected = qualify.runtime_output(b"\n".join(bodies), 1, "search-startup-log")["lineContext"]
            self.assertEqual(parse.call_count, len(selected["samples"]))
            self.assertEqual(parse.call_count, 16)
            self.assertEqual([sample["lineOrdinal"] for sample in selected["samples"]], list(range(1, 9)) + list(range(13, 21)))
        for body, source in ((frame_body, "broker-startup-log"), (frame_body, "readiness-command"), (b"\xff\n" + frame_body, "search-startup-log"), (b"x" * 4097, "search-startup-log"), ("\n".join(known).encode() + b"\n" + frame_body, "search-startup-log")):
            with mock.patch.object(qualify, "runtime_stackframe") as parse:
                qualify.runtime_output(body, 1, source)
                parse.assert_not_called()
        for source in ("broker-startup-log", "readiness-command"):
            other = qualify.runtime_output(secret.encode(), 1, source)
            self.assertNotIn("format", other)
            self.assertNotIn("samples", other["lineContext"])
            self.assertNotIn("unknownProjection", other["lineContext"])
        # This reducer is source-free and runs before registry authentication.
        # It has no body/symbol/unknown fingerprint or generic runtime-log seam.
        public = b"Caused by: java.sql.SQLException: " + secret.encode()
        reduced = qualify.reduce_failed_upgrade(public + b"\n" + public)
        self.assertEqual(reduced, {"outcome": "reduced", "signals": [{"code": "sql-error", "count": 2}]})
        self.assertNotIn(secret, json.dumps(reduced))
        self.assertNotIn("java.sql.SQLException", json.dumps(reduced))
        plain_report = ("2026-11-15 12:00:00,000 [main] ERROR c.l.d.u.DefaultUpgradeReport - Failed Step " + secret + "\n"
                        "2026-11-15 12:00:00,001 [main] ERROR c.l.d.u.DefaultUpgradeReport - Caught exception during attempt " + secret).encode()
        self.assertEqual(qualify.reduce_failed_upgrade(plain_report), {"outcome": "reduced", "signals": [{"code": "upgrade-failure-report", "count": 2}]})
        maximum = qualify.reduce_failed_upgrade(b"\n".join([public] * 256))
        self.assertEqual(maximum, {"outcome": "reduced", "signals": [{"code": "sql-error", "count": 256}]})
        self.assertEqual(qualify.reduce_failed_upgrade(b""), {"outcome": "empty-output", "signals": []})
        self.assertEqual(qualify.reduce_failed_upgrade(b"Exiting upgrade"), {"outcome": "unclassified-output", "signals": []})
        report_prefix = "2026-11-15 12:00:00,000 [main] INFO  c.l.d.u.DefaultUpgradeReport - "
        def step_codes(body):
            result = qualify.reduce_failed_upgrade(body.encode())
            self.assertEqual(set(result), {"outcome", "signals"})
            self.assertTrue(all(set(signal) == {"code", "count"} for signal in result["signals"]))
            self.assertNotIn(secret, json.dumps(result))
            return {signal["code"]: signal["count"] for signal in result["signals"] if signal["code"].startswith(("required-final-", "caught-attempt-"))}
        for step_id, code in qualify.FAILED_UPGRADE_STEP_CODES.items():
            final = "Failed Step 1/20: " + step_id + ". Failed after 0 retries."
            attempt = "Caught exception during attempt 0 of Step with id " + step_id
            self.assertEqual(step_codes(final), {"required-final-" + code: 1})
            self.assertEqual(step_codes(report_prefix + final), {"required-final-" + code: 1})
            self.assertEqual(step_codes(report_prefix + attempt), {"caught-attempt-" + code: 1})
            self.assertNotIn(step_id, json.dumps(qualify.reduce_failed_upgrade((report_prefix + final).encode())))
            for wrong_id in (step_id + "Suffix", step_id + "/private", "private/" + step_id, secret, "CreateUserStep", "SQLCreateTablesStep", "bootstrap-root-user-v6"):
                self.assertEqual(step_codes("Failed Step 1/20: " + wrong_id + ". Failed after 0 retries."), {})
                self.assertEqual(step_codes("Caught exception during attempt 0 of Step with id " + wrong_id), {})
            for invalid in ("Failed Step 0/20: " + step_id + ". Failed after 0 retries.", "Failed Step 2/1: " + step_id + ". Failed after 0 retries.", "Failed Step 01/20: " + step_id + ". Failed after 0 retries.", "Failed Step 1/0: " + step_id + ". Failed after 0 retries.", final + " " + secret, "/private/" + final, "Optional " + final, "Successfully completed " + step_id, "Caught exception during attempt -1 of Step with id " + step_id, "Caught exception during attempt 00 of Step with id " + step_id, "Caught exception during attempt 2147483648 of Step with id " + step_id, attempt + ": " + secret):
                self.assertEqual(step_codes(invalid), {})
            for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
                self.assertEqual(step_codes("Failed Step 1/20: " + separator + step_id + ". Failed after 0 retries."), {})
                self.assertEqual(step_codes("Caught exception during attempt 0 of Step with id " + separator + step_id), {})
        self.assertEqual(step_codes("Caught exception during attempt 2147483647 of Step with id CreateKafkaTopicsStep"), {"caught-attempt-create-kafka-topics": 1})
        self.assertEqual(step_codes("Failed Step 2147483647/2147483647: CreateKafkaTopicsStep. Failed after 2147483647 retries."), {"required-final-create-kafka-topics": 1})
        union_overflow = "\n".join("Failed Step 1/20: " + step_id + ". Failed after 0 retries." for step_id in list(qualify.FAILED_UPGRADE_STEP_CODES)[:16])
        self.assertEqual(qualify.reduce_failed_upgrade(union_overflow.encode()), {"outcome": "record-budget", "signals": []})
        for category, literals in qualify.FAILED_UPGRADE_LITERALS.items():
            for literal in literals:
                one = qualify.reduce_failed_upgrade((literal + ": " + secret).encode())
                self.assertEqual(set(one), {"outcome", "signals"})
                self.assertEqual(one["outcome"], "reduced")
                self.assertTrue(all(set(signal) == {"code", "count"} for signal in one["signals"]))
                self.assertIn({"code": category, "count": 1}, one["signals"])
                self.assertNotIn(secret, json.dumps(one))
                self.assertNotIn(literal, json.dumps(one))
        private_class = "com.private.credentials.TokenAlphaException"
        for body in (private_class.encode(), b"java.sql.SQLExceptionToken", b"private.java.sql.SQLException", b"/private/" + secret.encode() + b"/java.sql.SQLException", b"C:\\private\\java.sql.SQLException", b"java.sql.SQLException/private", b"JAVA.SQL.SQLEXCEPTION", secret.encode()):
            self.assertEqual(qualify.reduce_failed_upgrade(body), {"outcome": "unclassified-output", "signals": []})
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            self.assertEqual(qualify.reduce_failed_upgrade(("java.sql.SQL" + separator + "Exception").encode())["signals"], [])
        for body, outcome in ((b"\xff\n" + public, "malformed-output"), (b"\x1b[31m" + public + b"\x1b[0m", "malformed-output"), (public + b"\x00", "malformed-output"), (b"x" * (qualify.READINESS_OUTPUT_LIMIT + 1), "output-budget"), (b"\n".join([public] * 257), "line-budget"), (secret, "malformed-output")):
            self.assertEqual(qualify.reduce_failed_upgrade(body), {"outcome": outcome, "signals": []})
        with mock.patch.object(qualify, "FAILED_UPGRADE_LITERALS", {"fixed-" + str(index): ("PublicToken" + str(index),) for index in range(17)}):
            self.assertEqual(qualify.reduce_failed_upgrade("\n".join("PublicToken" + str(index) for index in range(17)).encode()), {"outcome": "record-budget", "signals": []})
        public_sources = qualify.datahub_frame_inventory()
        (outer, filename), (source_id, line_bound) = next((key, value) for key, value in public_sources.items() if value[1] >= 32)
        private_identifier = "CredentialMarkerAlpha"
        def source_frames(body):
            with mock.patch.object(qualify, "datahub_frame_inventory", return_value=public_sources):
                projected = qualify.reduce_failed_upgrade_frames(body)
            self.assertEqual(set(projected), {"outcome", "frames", "truncated"})
            self.assertTrue(all(set(frame) == {"sourceID", "claimedLine"} for frame in projected["frames"]))
            for private in (secret, private_identifier, outer, filename):
                self.assertNotIn(private, json.dumps(projected))
            return projected
        def public_frame(number=1, prefix="", nested=""):
            return (prefix + "at " + outer + nested + "." + private_identifier + "(" + filename + ":" + str(number) + ")").encode()
        expected_frame = {"sourceID": source_id, "claimedLine": 1}
        for body in (public_frame(), public_frame(prefix="\t"), public_frame(prefix=" \t  "), public_frame().replace(b"at ", b"at app//", 1), public_frame(nested="$" + private_identifier + "$1"), public_frame() + (" [" + private_identifier + ":" + private_identifier + "]").encode(), public_frame() + (" ~[" + private_identifier + ":" + private_identifier + "]").encode()):
            self.assertEqual(source_frames(body), {"outcome": "projected", "frames": [expected_frame], "truncated": False})
        self.assertEqual(source_frames(public_frame() + b"\n" + public_frame())["frames"], [expected_frame])
        sixteen = b"\n".join(public_frame(number) for number in range(1, 17))
        self.assertEqual(len(source_frames(sixteen)["frames"]), 16)
        self.assertFalse(source_frames(sixteen)["truncated"])
        seventeen = source_frames(sixteen + b"\n" + public_frame(17))
        self.assertEqual(len(seventeen["frames"]), 16)
        self.assertTrue(seventeen["truncated"])
        self.assertEqual([frame["claimedLine"] for frame in seventeen["frames"]], list(range(1, 17)))
        wrong_frames = [public_frame().replace(outer.encode(), b"private.ValidCredentialClass"), public_frame().replace(filename.encode(), b"Private.java"), public_frame().replace(filename.encode(), b"private/" + filename.encode()), public_frame().replace(b"at ", b"at java.base/", 1), public_frame().replace(b"at ", b"at loader/app//", 1), b" " * 17 + public_frame(), public_frame() + private_identifier.encode(), public_frame().replace(b":1)", b":01)"), public_frame(0), public_frame(-1), public_frame(2147483648), public_frame(line_bound + 1), public_frame().replace((filename + ":1").encode(), b"Native Method"), public_frame().replace((filename + ":1").encode(), b"Unknown Source"), public_frame() + b" [private:version", public_frame() + b" [private:version]trailing", public_frame() + b" [private]", public_frame() + b" [[private:version]]"]
        for body in wrong_frames:
            self.assertEqual(source_frames(body), {"outcome": "no-known-frame", "frames": [], "truncated": False})
        for separator in ("\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            self.assertEqual(source_frames(public_frame().replace(b"(" + filename.encode(), separator.encode() + b"(" + filename.encode()))["frames"], [])
        for body, outcome in ((b"", "empty-output"), (b"\xff" + public_frame(), "malformed-output"), (b"\x1b[31m" + public_frame(), "malformed-output"), (public_frame() + b"\x00", "malformed-output"), (b"x" * (qualify.READINESS_OUTPUT_LIMIT + 1), "output-budget"), (b"\n".join([public_frame()] * 257), "line-budget")):
            self.assertEqual(source_frames(body), {"outcome": outcome, "frames": [], "truncated": False})
        manifest = json.loads((qualify.HERE / "datahub-source-frames.json").read_text())
        for mutation in ("identity", "row-id", "class", "path", "blob", "size", "count", "provenance", "duplicate", "extra"):
            changed = json.loads(json.dumps(manifest))
            row = changed["sources"][0]
            if mutation == "identity": changed["commit"] = "c" * 40
            elif mutation == "row-id": row["id"] = True
            elif mutation == "class": row["className"] = "private." + private_identifier
            elif mutation == "path": row["path"] = "datahub-upgrade/src/main/java/../Private.java"
            elif mutation == "blob": row["blobSHA"] = secret
            elif mutation == "size": row["bytes"] = 0
            elif mutation == "count": changed["components"][0]["sourceCount"] += 1
            elif mutation == "provenance": changed["provenance"]["allUntruncated"] = 1
            elif mutation == "duplicate": changed["sources"][1] = row
            else: row["private"] = secret
            with mock.patch.object(qualify, "bounded_file", return_value=json.dumps(changed).encode()):
                refused_frames = qualify.reduce_failed_upgrade_frames(public_frame())
            self.assertEqual(refused_frames, {"outcome": "source-inventory-refused", "frames": [], "truncated": False})
            self.assertNotIn(secret, json.dumps(refused_frames))
        for body, code, source in ((runtime, secret, "broker-startup-log"), (runtime, 1, secret), (runtime, 1, "search-log"), (runtime, 1, "mysql-startup-log"), (runtime, 1, "gms-startup-log"), (runtime, 1, "upgrade-startup-log"), (b"x" * (qualify.READINESS_OUTPUT_LIMIT + 1), 1, "broker-startup-log"), (b"x" * (qualify.READINESS_OUTPUT_LIMIT + 1), 1, "search-startup-log")):
            try:
                qualify.runtime_output(body, code, source)
            except ValueError as error:
                refused = str(error) + traceback.format_exc()
            else:
                self.fail("unknown/oversized runtime projection must refuse")
            self.assertNotIn(secret, refused)



if __name__ == "__main__":
    unittest.main()
