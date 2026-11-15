"""Discriminate source, partial-create cleanup and non-secret failure evidence."""
import copy
import fnmatch
import json
import os
import subprocess
import sys
import tempfile
import traceback
import unittest
from pathlib import Path
from unittest import mock

import qualify
import yaml

IDENTITY = {"sourceSHA": "a" * 40, "treeSHA": "b" * 40, "runID": "123", "runAttempt": "1"}
OWNER_PATH = Path("/owned/run/catalog-owner.json")
OWNER = dict(IDENTITY, schema=1, kind="catalog-qualification", nonce="0123456789abcdef", network="kanz-catalog-0123456789abcdef",
             scannerMount={"path": "/owned/run/catalog-fixture/scanner-state", "source": "kanz-catalog-0123456789abcdef-scanner", "filesystem": "tmpfs", "bytes": qualify.SCANNER_DISK_LIMIT, "options": ["nosuid", "nodev", "noexec"], "uid": os.getuid(), "gid": os.getgid()})
OWNER["derivedImage"] = qualify.image_patch.owned_spec(OWNER, OWNER_PATH)
OWNER.update(registryProfile="/owned/run/catalog-registry-" + IDENTITY["sourceSHA"][:12] + "-123-1-0123456789abcdef", registryHelperSHA256="7" * 64, registryControlsSHA256="8" * 64, buildDiagnosticControlsSHA256="9" * 64)


def runtime_state_info(role, status="running"):
    return {"Id": "6" * 64, "Name": "/" + OWNER["network"] + "-" + role, "Image": "sha256:" + "7" * 64,
            "Config": {"Labels": qualify.labels(OWNER)}, "HostConfig": {"NetworkMode": OWNER["network"]},
            "State": {"Status": status, "Running": status == "running", "Paused": False, "Restarting": False,
                      "OOMKilled": False, "Dead": status == "dead", "ExitCode": 1 if status == "exited" else 0, "Error": ""}}


class QualificationControls(unittest.TestCase):
    def test_search_java_variant_preserves_provider_and_isolation_contract(self):
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network = OWNER, OWNER["network"]
        reference = "opensearchproject/opensearch:3.9.0@sha256:5c20af6b5cb472a74ba99f663bee3dd03afbeced14b3897b220a87fc75278445"
        fixture.images, fixture.run = {"search": reference}, mock.Mock(return_value="")
        self.assertEqual(fixture.start_search(), OWNER["network"] + "-search")
        args = fixture.run.call_args.args[0]
        image_index = args.index(reference)
        self.assertEqual(qualify.image_identity.selections()["search"]["reference"], reference)
        self.assertEqual(args[image_index + 1:], ["-c", qualify.SEARCH_CONFIG_PREPARE, "catalog-search-config", "/usr/share/opensearch/config", "/tmp/kanz-catalog-search-config", "/usr/share/opensearch/bin/opensearch", "-Ediscovery.type=single-node", "-Eplugins.security.disabled=true", "-Enetwork.host=0.0.0.0", "-Enode.store.allow_mmap=false"])
        options = args[:image_index]
        def values(flag):
            return [options[index + 1] for index, value in enumerate(options[:-1]) if value == flag]
        self.assertEqual(values("--env"), ["OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m -Dorg.bouncycastle.native.cpu_variant=java -Dopensearch.logs.base_path=/usr/share/opensearch/logs"])
        self.assertEqual(values("--entrypoint"), ["/bin/bash"])
        self.assertEqual(values("--user"), ["1000:1000"])
        self.assertEqual(values("--tmpfs"), ["/tmp:rw,nosuid,nodev,size=512m", "/usr/share/opensearch/data:rw,nosuid,nodev,size=1g,uid=1000,gid=1000", "/usr/share/opensearch/logs:rw,nosuid,nodev,size=64m,uid=1000,gid=1000"])
        for flag, expected in (("--cap-drop", "ALL"), ("--security-opt", "no-new-privileges"), ("--network", OWNER["network"]), ("--memory", "2g"), ("--memory-swap", "2g"), ("--cpus", "2"), ("--pids-limit", "256"), ("--platform", "linux/amd64")):
            self.assertEqual(values(flag), [expected])
        self.assertIn("--read-only", options)
        for flag in ("--privileged", "--cap-add", "--mount", "--volume", "--publish", "-p"):
            self.assertNotIn(flag, options)
        self.assertNotIn(OWNER["registryProfile"], " ".join(args))
        self.assertFalse(any("approved_only" in value or "enable-native-access" in value or "java.security.properties" in value for value in args))

    def test_search_config_preparation_is_exclusive_and_preserves_relative_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "vendor-config"
            source.mkdir()
            (source / "jvm.options.d").mkdir()
            (source / ".empty").mkdir()
            copy_tools = root / "copy-tools"
            copy_tools.mkdir()
            for command in ("mkdir", "cp"):
                (copy_tools / command).symlink_to("/bin/" + command)
            contents = {"opensearch.yml": b"cluster.name: fixture\n", ".fixture": b"hidden\n", "jvm.options.d/fixture.options": b"-Xms512m\n"}
            for name, body in contents.items():
                (source / name).write_bytes(body)
            stub = 'import json,os,sys; print(json.dumps({"config":os.environ["OPENSEARCH_PATH_CONF"],"args":sys.argv[1:]}))'
            arguments = ["-Ediscovery.type=single-node", "literal *", "one argument with spaces"]
            def prepare(destination, selected_source=source, tools=copy_tools):
                return subprocess.run(["/bin/bash", "-c", qualify.SEARCH_CONFIG_PREPARE, "catalog-search-config", str(selected_source), str(destination), sys.executable, "-c", stub, *arguments], env={"PATH": str(tools)}, capture_output=True, text=True, timeout=5)
            destination = root / "fresh"
            result = prepare(destination)
            self.assertEqual(result.returncode, 0)
            self.assertEqual(json.loads(result.stdout), {"config": str(destination), "args": arguments})
            self.assertEqual(destination.stat().st_mode & 0o777, 0o700)
            self.assertTrue((destination / ".empty").is_dir())
            for name, body in contents.items():
                self.assertEqual((destination / name).read_bytes(), body)
                self.assertEqual((source / name).read_bytes(), body)
            stale = destination / "stale"
            stale.write_bytes(b"preserve")
            result = prepare(destination)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, "")
            self.assertEqual(stale.read_bytes(), b"preserve")
            linked_destination = root / "linked-destination"
            linked_destination.symlink_to(destination, target_is_directory=True)
            linked_source = root / "linked-source"
            linked_source.symlink_to(source, target_is_directory=True)
            for copy, original in ((linked_destination, source), (root / "copy-linked-source", linked_source), (root / "copy-missing-source", root / "missing-source"), (root / "copy-file-source", source / "opensearch.yml")):
                result = prepare(copy, original)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
            self.assertFalse((root / "copy-linked-source").exists())
            self.assertFalse((root / "copy-missing-source").exists())
            self.assertFalse((root / "copy-file-source").exists())
            for name, target in (("linked-file", source / "opensearch.yml"), ("linked-directory", source / "jvm.options.d"), ("\n\n", source / "opensearch.yml"), ("jvm.options.d/.linked", source / "opensearch.yml")):
                link = source / name
                link.symlink_to(target, target_is_directory=target.is_dir())
                refused = root / "copy-linked-entry"
                result = prepare(refused)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertFalse(refused.exists())
                link.unlink()
            for name in ("special-entry", "\n"):
                fifo = source / name
                os.mkfifo(fifo)
                result = prepare(root / "copy-special")
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertFalse((root / "copy-special").exists())
                fifo.unlink()
            unreadable = source / "jvm.options.d/unreadable"
            unreadable.mkdir()
            unreadable.chmod(0)
            try:
                result = prepare(root / "unreadable-copy")
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertFalse((root / "unreadable-copy").exists())
            finally:
                unreadable.chmod(0o700)
                unreadable.rmdir()
            tools = root / "cp-failure"
            tools.mkdir()
            (tools / "cp").symlink_to("/bin/false")
            result = prepare(root / "cp-copy", tools=str(tools) + ":" + str(copy_tools))
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, "")

    def test_broker_rpc_and_live_property_require_the_same_owned_container(self):
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network = OWNER, OWNER["network"]
        fixture.images = {"broker": qualify.image_identity.selections()["broker"]["reference"]}
        fixture.run = mock.Mock(return_value="")
        name = fixture.start_broker()
        self.assertEqual(name, OWNER["network"] + "-broker")
        args = fixture.run.call_args.args[0]
        image_index = args.index(fixture.images["broker"])
        self.assertEqual(args[image_index + 1:], ["redpanda", "start", "--config", "/tmp/redpanda.yaml", "--overprovisioned", "--smp", "1", "--memory", "512M", "--reserve-memory", "0M", "--node-id", "0", "--check=false", "--kafka-addr", "0.0.0.0:9092", "--advertise-kafka-addr", "broker:9092", "--advertise-rpc-addr", "broker:33145", "--set", "redpanda.auto_create_topics_enabled=true"])
        docker_options = args[:image_index]
        for flag, expected in (("--cap-drop", "ALL"), ("--security-opt", "no-new-privileges"), ("--network", OWNER["network"]), ("--memory", "1g"), ("--memory-swap", "1g"), ("--cpus", "2"), ("--pids-limit", "256")):
            self.assertEqual(docker_options[docker_options.index(flag) + 1], expected)
        self.assertIn("--read-only", docker_options)
        self.assertEqual([docker_options[index + 1] for index, value in enumerate(docker_options[:-1]) if value == "--tmpfs"], ["/tmp:rw,nosuid,nodev,size=512m", "/var/lib/redpanda/data:rw,nosuid,nodev,size=1g,uid=101,gid=101"])
        self.assertNotIn(OWNER["registryProfile"], " ".join(args))
        secret, container_id = "one-run-broker-control-marker", "6" * 64
        fixture.stage = "broker-live-configuration"
        fixture.image_identities = {"broker": {"imageID": "sha256:" + "7" * 64}}
        calls, inspections, output, failure = [], [], "true\n", None
        def execute(command, **kwargs):
            calls.append(command)
            if command[:2] == ["docker", "inspect"]:
                selected = inspections.pop(0) if len(inspections) > 1 else inspections[0]
                return json.dumps([selected])
            self.assertEqual(command[:6], ["docker", "exec", container_id, "rpk", "cluster", "config"])
            self.assertEqual(command[7], "auto_create_topics_enabled")
            self.assertEqual(command[-3:], ["--ignore-profile", "-X", "admin.hosts=127.0.0.1:9644"])
            operation = command[6]
            self.assertEqual(command, ["docker", "exec", container_id, "rpk", "cluster", "config", operation, "auto_create_topics_enabled"] + (["true"] if operation == "set" else []) + ["--ignore-profile", "-X", "admin.hosts=127.0.0.1:9644"])
            self.assertEqual(kwargs, {"timeout": 30, "output_limit": 4096 if operation == "set" else 64})
            if failure == operation:
                raise ValueError(secret)
            if operation == "set":
                self.assertEqual(command[8], "true")
                return secret
            self.assertEqual(operation, "get")
            return output
        fixture.run = execute
        inspections[:] = [runtime_state_info("broker")]
        fixture.configure_broker(name)
        passed = copy.deepcopy(fixture.broker_evidence)
        qualify.validate_broker_runtime(passed, fixture.image_identities["broker"]["imageID"], OWNER["sourceSHA"])
        self.assertEqual([command[-1] for command in calls if command[:2] == ["docker", "inspect"]], [name, container_id, container_id])
        self.assertEqual([command[6] for command in calls if command[:2] == ["docker", "exec"]], ["set", "get"])
        self.assertNotIn(secret, json.dumps(passed))
        for output in ("", "false\n", "true", "true\r\n", "true\n\n", " true\n", "True\n", '"true"\n', "true\n" + secret):
            calls.clear()
            with self.assertRaises(ValueError):
                fixture.configure_broker(name)
            self.assertIsNone(fixture.broker_evidence)
        output = "true\n"
        for failure in ("set", "get"):
            calls.clear()
            try:
                fixture.configure_broker(name)
            except ValueError as error:
                self.assertNotIn(secret, str(error) + traceback.format_exc())
            else:
                self.fail("failed broker Admin API command must refuse")
            self.assertIsNone(fixture.broker_evidence)
            if failure == "set":
                self.assertFalse(any(command[:2] == ["docker", "exec"] and command[6] == "get" for command in calls))
        failure = None
        for field, value in (("Image", "sha256:" + "f" * 64), ("Name", "/foreign"), ("Id", secret), ("Config", {"Labels": {"foreign": secret}}), ("HostConfig", {"NetworkMode": "foreign"}), ("State", runtime_state_info("broker", "exited")["State"])):
            calls.clear()
            inspections[:] = [dict(runtime_state_info("broker"), **{field: value})]
            with self.assertRaises(ValueError):
                fixture.configure_broker(name)
            self.assertFalse(any(command[:2] == ["docker", "exec"] for command in calls))
            self.assertIsNone(fixture.broker_evidence)
        for replacement in (dict(runtime_state_info("broker"), Id="8" * 64), dict(runtime_state_info("broker"), Config={"Labels": {"foreign": secret}}), dict(runtime_state_info("broker"), Name="/foreign")):
            calls.clear()
            inspections[:] = [runtime_state_info("broker"), replacement]
            with self.assertRaises(ValueError):
                fixture.configure_broker(name)
            self.assertEqual([command[6] for command in calls if command[:2] == ["docker", "exec"]], ["set"])
            self.assertIsNone(fixture.broker_evidence)
        calls.clear()
        inspections[:] = [runtime_state_info("broker"), runtime_state_info("broker"), dict(runtime_state_info("broker"), Name="/foreign")]
        with self.assertRaises(ValueError):
            fixture.configure_broker(name)
        self.assertEqual([command[6] for command in calls if command[:2] == ["docker", "exec"]], ["set", "get"])
        self.assertIsNone(fixture.broker_evidence)
        calls.clear()
        with self.assertRaises(ValueError):
            fixture.configure_broker(OWNER["network"] + "-mysql")
        self.assertEqual(calls, [])
        for evidence in (None, {}, *(dict(passed, **{key: value}) for key, value in (("value", False), ("value", 1), ("sourceSHA", "c" * 40), ("imageID", "sha256:" + "f" * 64), ("containerID", "foreign"), ("readbackBytes", 0), ("readbackSHA256", "f" * 64), ("setSucceeded", False), ("getSucceeded", False), ("ownerVerified", False), ("private", secret)))):
            with self.assertRaises(ValueError):
                qualify.validate_broker_runtime(evidence, fixture.image_identities["broker"]["imageID"], OWNER["sourceSHA"])

    def test_readiness_diagnostics_bind_owner_limit_retention_and_preserve_refusal(self):
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network, fixture.stage = OWNER, OWNER["network"], "storage-readiness"
        fixture.image_identities = {role: {"imageID": "sha256:" + "7" * 64} for role in qualify.READINESS_ROLES}
        name, container_id = OWNER["network"] + "-broker", "6" * 64
        secret = "one-run-runtime-credential-marker"
        info = runtime_state_info("broker", "exited")
        info["Config"]["Env"] = ["PASSWORD=" + secret]
        info["State"].update(Error=secret, Health={"Status": "unhealthy", "Log": [{"Output": secret}]})
        calls = []
        inspected = info
        name_replaced = False
        log_source = "broker-startup-log"
        log_body = "unknown flag " + secret + "\nwrite /tmp/redpanda.yaml: permission denied " + secret
        def execute(args, **kwargs):
            nonlocal name_replaced
            calls.append((args, kwargs))
            self.assertEqual(kwargs["output_limit"], 65536)
            if args[:2] == ["docker", "exec"]:
                self.assertEqual(args[2], container_id)
                name_replaced = True
                fixture.last_runtime_result = qualify.runtime_output(("connection refused " + secret).encode(), 1, "readiness-command")
                raise qualify.CommandFailure(fixture.stage, "command-exit", 1)
            if args[:2] == ["docker", "inspect"]:
                if args[-1] == name and name_replaced:
                    return json.dumps([dict(info, Id="8" * 64, Config={"Labels": {"foreign": secret}})])
                return json.dumps([inspected])
            if args[:4] == ["docker", "logs", "--tail", "40"]:
                self.assertEqual(args[-1], container_id)
                self.assertEqual(kwargs["runtime_source"], log_source)
                fixture.last_runtime_result = qualify.runtime_output(log_body.encode(), 0, log_source)
                return secret
            self.fail("diagnostics cannot launch or mutate resources")
        fixture.run = execute
        try:
            fixture.wait_exec(name, ["rpk", "cluster", "info"])
        except ValueError as error:
            refusal = str(error) + traceback.format_exc()
        else:
            self.fail("terminal broker diagnostics cannot satisfy readiness")
        self.assertNotIn(secret, refusal)
        entry = fixture.readiness_evidence["broker"]
        self.assertEqual(entry["waitOutcome"], "terminal-state")
        self.assertEqual(entry["recent"][0]["container"]["exitCode"], 1)
        self.assertTrue(entry["recent"][0]["container"]["errorPresent"])
        self.assertEqual(entry["recent"][0]["brokerLog"]["matchedSignals"], ["option-refused", "permission-refused"])
        context = entry["recent"][0]["brokerLog"]["lineContext"]
        self.assertEqual((context["signalLines"], context["omittedSignalLines"]), (2, 0))
        selected = next(record for record in context["records"] if record["resource"] == "selected-broker-config")
        self.assertEqual((selected["operation"], selected["error"], selected["count"]), ("write", "permission-denied", 1))
        self.assertEqual(selected["errnoSignals"], ["permission-denied"])
        self.assertEqual(selected["coOccurrence"], {"resourceSignals": ["selected-broker-config"], "apiSignals": [], "templateSignals": []})
        self.assertEqual(entry["recent"][0]["binding"]["containerID"], container_id)
        self.assertEqual([args[-1] for args, _ in calls if args[:2] == ["docker", "inspect"]], [name, container_id])
        self.assertNotIn(secret, json.dumps(fixture.readiness_evidence))
        for field, value in (("Image", "sha256:" + "f" * 64), ("Name", "/foreign"), ("Id", "foreign"), ("Config", {"Labels": {}})):
            calls.clear()
            name_replaced = False
            fixture.readiness_evidence = {}
            inspected = dict(info, **{field: value})
            with self.assertRaises(ValueError):
                fixture.wait_exec(name, ["rpk", "cluster", "info"])
            self.assertFalse(any(args[:2] in (["docker", "logs"], ["docker", "exec"]) for args, _ in calls))
            refused = json.dumps(fixture.readiness_evidence)
            for unbound in ("capturedSHA256", "matchedSignals", "lineContext", "probe", secret):
                self.assertNotIn(unbound, refused)
        name = OWNER["network"] + "-search"
        info = runtime_state_info("search", "exited")
        info["Config"]["Env"] = ["PASSWORD=" + secret]
        info["State"]["Error"] = secret
        inspected, name_replaced = info, False
        log_source = "search-startup-log"
        log_body = 'Likely root cause: java.nio.file.AccessDeniedException: "/usr/share/opensearch/config/opensearch.keystore.tmp" ' + secret
        fixture.readiness_evidence = {}
        calls.clear()
        with self.assertRaises(ValueError):
            fixture.wait_exec(name, ["curl", "--fail", "--silent", "http://search:9200/_cluster/health"])
        entry = fixture.readiness_evidence["search"]
        self.assertEqual(entry["waitOutcome"], "terminal-state")
        self.assertEqual([args[-1] for args, _ in calls if args[:2] == ["docker", "inspect"]], [name, container_id])
        self.assertNotIn("brokerLog", entry["recent"][0])
        observed = entry["recent"][0]["searchLog"]
        self.assertEqual(observed["matchedSignals"], ["search-access-denied-exception", "search-root-cause-wrapper"])
        self.assertEqual(observed["lineContext"]["records"][0]["coOccurrence"]["resourceSignals"], ["packaged-search-config"])
        self.assertIsNone(observed["lineContext"]["records"][0]["startupMessage"])
        self.assertNotIn(secret, json.dumps(fixture.readiness_evidence))
        for field, value in (("Image", "sha256:" + "f" * 64), ("Name", "/foreign"), ("Id", "foreign"), ("Config", {"Labels": {}})):
            calls.clear()
            name_replaced = False
            fixture.readiness_evidence = {}
            inspected = dict(info, **{field: value})
            with self.assertRaises(ValueError):
                fixture.wait_exec(name, ["curl", "--fail", "--silent", "http://search:9200/_cluster/health"])
            self.assertFalse(any(args[:2] in (["docker", "logs"], ["docker", "exec"]) for args, _ in calls))
            for unbound in ("capturedSHA256", "matchedSignals", "lineContext", "probe", secret):
                self.assertNotIn(unbound, json.dumps(fixture.readiness_evidence))
        for role in ("mysql", "gms"):
            calls.clear()
            inspected = dict(info, Name="/" + OWNER["network"] + "-" + role)
            binding = qualify.owned_runtime_state(inspected, OWNER["network"] + "-" + role, "sha256:" + "7" * 64, OWNER["network"], qualify.labels(OWNER))
            fixture.readiness_failure(OWNER["network"] + "-" + role, binding, qualify.runtime_refusal("timeout", "readiness-command"))
            self.assertFalse(any(args[:2] == ["docker", "logs"] for args, _ in calls))
        calls.clear()
        with self.assertRaises(ValueError):
            fixture.wait_exec("foreign-broker", ["rpk", "cluster", "info"])
        self.assertEqual(calls, [])
        for index in range(30):
            qualify.retain_readiness(fixture.readiness_evidence, "broker", {"attempt": index})
        self.assertEqual(len(fixture.readiness_evidence["broker"]["recent"]), 8)
        self.assertEqual(fixture.readiness_evidence["broker"]["recent"][0], {"attempt": 22})
        self.assertNotIn(secret, json.dumps(fixture.readiness_evidence))
        name = OWNER["network"] + "-broker"
        with mock.patch.object(qualify.time, "monotonic", side_effect=[0, 1]), self.assertRaises(ValueError):
            fixture.wait_exec(name, ["rpk", "cluster", "info"], timeout=1)
        self.assertEqual(fixture.readiness_evidence["broker"]["waitOutcome"], "deadline-exceeded")
        bounded = bytearray(b"x" * 65536)
        with self.assertRaises(qualify.CommandFailure):
            qualify.append_output(bounded, b"x", 65536)
        self.assertEqual(len(bounded), 65536)
        runner = object.__new__(qualify.Fixture)
        runner.stage, runner.environment = "storage-readiness", {}
        process = mock.Mock()
        for source in ("broker-startup-log", "search-startup-log"):
            with mock.patch.object(qualify.subprocess, "Popen", return_value=process), mock.patch.object(qualify, "capture_process", side_effect=qualify.CommandFailure("capture", "output-budget")) as capture, mock.patch.object(qualify, "stop_process") as stop:
                with self.assertRaises(qualify.CommandFailure):
                    runner.run(["docker", "logs", "--tail", "40", container_id], output_limit=65536, runtime_source=source)
                capture.assert_called_once_with(process, 180, 65536)
                stop.assert_called_once_with(process)
            self.assertEqual(runner.last_runtime_result["category"], "output-budget")
            self.assertFalse(runner.last_runtime_result["captureComplete"])
            self.assertIsNone(runner.last_runtime_result["capturedSHA256"])
        with mock.patch.object(qualify.subprocess, "Popen") as launch, self.assertRaises(ValueError):
            runner.run(["docker", "inspect", container_id], output_limit=65536, runtime_source="search-startup-log")
        launch.assert_not_called()

    def test_mysql_startup_preserves_vendor_plan_and_bounded_nonroot_profile(self):
        fixture = object.__new__(qualify.Fixture)
        reference = "ubuntu/mysql:8.4-26.04_stable@sha256:b22e8fc950df66a17d4b001b28ef5b557b9572d06da25f93967ea3f455c6410e"
        fixture.owner, fixture.network = OWNER, OWNER["network"]
        fixture.images, fixture.secret = {"mysql": reference}, "one-run-mysql-control-marker"
        fixture.run = mock.Mock(return_value="")
        self.assertEqual(fixture.start_mysql(), OWNER["network"] + "-mysql")
        args = fixture.run.call_args.args[0]
        self.assertEqual(args[-1], reference)
        self.assertEqual(qualify.image_identity.selections()["mysql"]["reference"], reference)
        self.assertEqual(qualify.image_identity.selections()["mysql"]["configDigest"], "sha256:73b12c45d61f5005fe5252c2bbb65d75d5f0e5c3725ff0f97d25a35fa9169d4f")
        def values(flag):
            return [args[index + 1] for index, value in enumerate(args[:-1]) if value == flag]
        self.assertEqual(values("--user"), ["584788:584788"])
        self.assertEqual(values("--tmpfs"), ["/tmp:rw,nosuid,nodev,size=512m", "/var/lib/mysql:rw,nosuid,nodev,size=1g,uid=584788,gid=584788", "/run/mysqld:rw,nosuid,nodev,size=16m,uid=584788,gid=584788"])
        self.assertEqual(values("--env"), ["PEBBLE=/tmp/catalog-pebble", "PEBBLE_COPY_ONCE=/var/lib/pebble/default", "PEBBLE_PERSIST=never", "XDG_CONFIG_HOME=/tmp/catalog-xdg", "MYSQL_ROOT_PASSWORD=one-run-mysql-control-marker", "MYSQL_ROOT_HOST=%", "MYSQL_DATABASE=datahub"])
        for flag, expected in (("--cap-drop", "ALL"), ("--security-opt", "no-new-privileges"), ("--network", OWNER["network"]), ("--memory", "1g"), ("--memory-swap", "1g"), ("--cpus", "2"), ("--pids-limit", "256")):
            self.assertEqual(values(flag), [expected])
        self.assertIn("--read-only", args)
        for flag in ("--entrypoint", "--cap-add", "--privileged", "--publish", "-p", "--mount", "--volume"):
            self.assertNotIn(flag, args)
        self.assertNotIn(OWNER["registryProfile"], " ".join(args))
        # Actual process inspection and SQL networking distinguish the final
        # authenticated server from mysqladmin's temporary-init readiness.
        for expected in ("/proc/1/status", "= pebble", "= mysqld", "/run/mysqld/mysqld.pid", "@@GLOBAL.skip_networking", "MYSQL_PWD=\"$MYSQL_ROOT_PASSWORD\"", "pebbleUID=", "mysqlUID="):
            self.assertIn(expected, qualify.MYSQL_PROBE)
        wait = object.__new__(qualify.Fixture)
        wait.owner, wait.network = OWNER, OWNER["network"]
        wait.image_identities = {"mysql": {"imageID": "sha256:" + "7" * 64}}
        wait.run = mock.Mock(side_effect=[json.dumps([runtime_state_info("mysql")]), "public-observation"])
        self.assertEqual(wait.wait_exec(OWNER["network"] + "-mysql", ["bash", "-ec", qualify.MYSQL_PROBE]), "public-observation")

    def test_mysql_runtime_refuses_foreign_privileges_identity_and_secret_output(self):
        image_id = qualify.image_identity.selections()["mysql"]["manifestDigest"]
        info = {"Image": image_id, "Config": {"User": "584788:584788", "Entrypoint": ["/usr/bin/pebble", "enter"], "Cmd": None, "Labels": qualify.labels(OWNER),
                "Env": ["PEBBLE=/tmp/catalog-pebble", "PEBBLE_COPY_ONCE=/var/lib/pebble/default", "PEBBLE_PERSIST=never", "XDG_CONFIG_HOME=/tmp/catalog-xdg", "MYSQL_ROOT_HOST=%", "MYSQL_DATABASE=datahub", "MYSQL_ROOT_PASSWORD=one-run-secret-marker"]},
                "HostConfig": {"ReadonlyRootfs": True, "CapDrop": ["ALL"], "CapAdd": None, "Privileged": False, "SecurityOpt": ["no-new-privileges"], "NetworkMode": OWNER["network"], "PortBindings": {}, "PublishAllPorts": False, "Memory": 1073741824, "MemorySwap": 1073741824, "NanoCpus": 2000000000, "PidsLimit": 256,
                "Tmpfs": {"/tmp": "rw,nosuid,nodev,size=512m", "/var/lib/mysql": "rw,nosuid,nodev,size=1g,uid=584788,gid=584788", "/run/mysqld": "rw,nosuid,nodev,size=16m,uid=584788,gid=584788"}, "Binds": None, "Mounts": None}, "Mounts": []}
        profile = qualify.mysql_container_profile(info, image_id, OWNER["network"], qualify.labels(OWNER))
        self.assertNotIn("one-run-secret-marker", json.dumps(profile))
        changes = [("Config", "User", "999:999"), ("Config", "Entrypoint", ["/bin/sh"]), ("Config", "Cmd", ["mysqld"]), ("Config", "Labels", {}), ("Config", "Env", info["Config"]["Env"] + ["DOCKER_CONFIG=" + OWNER["registryProfile"]]), ("Config", "Env", info["Config"]["Env"] + ["PEBBLE=/foreign"]),
                   ("HostConfig", "ReadonlyRootfs", False), ("HostConfig", "CapDrop", []), ("HostConfig", "CapAdd", ["SYS_ADMIN"]), ("HostConfig", "Privileged", True), ("HostConfig", "SecurityOpt", []), ("HostConfig", "NetworkMode", "host"), ("HostConfig", "PortBindings", {"3306/tcp": [{"HostPort": "3306"}]}), ("HostConfig", "Memory", 0), ("HostConfig", "MemorySwap", -1), ("HostConfig", "NanoCpus", 0), ("HostConfig", "PidsLimit", 0), ("HostConfig", "Tmpfs", dict(info["HostConfig"]["Tmpfs"], **{"/etc/mysql": "rw"})), ("HostConfig", "Tmpfs", dict(info["HostConfig"]["Tmpfs"], **{"/run/mysqld": "rw,uid=999,gid=999"})), ("HostConfig", "Binds", ["/host:/target:rw"])]
        for group, key, value in changes:
            changed = copy.deepcopy(info)
            changed[group][key] = value
            with self.subTest(group=group, key=key), self.assertRaises(ValueError):
                qualify.mysql_container_profile(changed, image_id, OWNER["network"], qualify.labels(OWNER))
        with self.assertRaises(ValueError):
            qualify.mysql_container_profile(dict(info, Image="sha256:" + "f" * 64), image_id, OWNER["network"], qualify.labels(OWNER))
        output = "pebbleUID=584788\npebbleGID=584788\nmysqlUID=584788\nmysqlGID=584788\nsql=8.4.11-0ubuntu0.26.04.1\t0\n"
        observation = qualify.mysql_observation(output)
        qualify.validate_mysql_runtime({"container": profile, "observation": observation}, image_id)
        for changed in (output.replace("mysqlUID=584788", "mysqlUID=0"), output.replace("\t0", "\t1"), output.replace("8.4.11-0ubuntu0.26.04.1", "8.4.13"), output.replace("8.4.11-0ubuntu0.26.04.1", "one-run-secret-marker"), output + "one-run-secret-marker\n"):
            observed = {}
            try:
                observed = qualify.mysql_observation(changed)
                qualify.validate_mysql_runtime({"container": profile, "observation": observed}, image_id)
            except ValueError as error:
                diagnostic = json.dumps(observed) + str(error) + traceback.format_exc()
            else:
                self.fail("changed public runtime evidence must refuse")
            self.assertNotIn("one-run-secret-marker", diagnostic)
        for evidence in (None, {}, {"container": profile}, {"container": profile, "observation": None}, {"container": dict(profile, imageID="sha256:" + "f" * 64), "observation": observation}):
            with self.assertRaises(ValueError):
                qualify.validate_mysql_runtime(evidence, image_id)
        with self.assertRaises(ValueError):
            qualify.mysql_observation("one-run-secret-marker" * 4096)

    def test_consumed_inputs_select_both_pr_and_main_qualification(self):
        path = Path(os.environ.get("KANZ_CATALOG_WORKFLOW", qualify.ROOT / ".github/workflows/catalog-verification.yml"))
        # BaseLoader retains the Actions `on` key rather than YAML 1.1's bool.
        workflow = yaml.load(path.read_text(), Loader=yaml.BaseLoader)
        consumed = ("kanz-jl/qualification/install-audit.sh", "kanz/pkg/auth/audit.go",
                    "kanz/pkg/authbus/recorder.go", "kanz/pkg/bus/validate.go",
                    "kanz/pkg/decisionq/queue.go", "kanz/pkg/observability/observability.go",
                    "kanz/pkg/transport/tls.go", "kanz/internal/bustest/bustest.go",
                    "kanz/internal/env/env.go", "kanz/internal/lifecycle/fatal.go",
                    "kanz/internal/platform/httpserver/httpserver.go", "kanz/internal/version/version.go",
                    "kanz/test/arch/upstream_principal_reconstruction_test.go",
                    "kanz-schemas/proto/envelope/v1/envelope.proto",
                    "kanz-schemas/proto/observation/v1/observation.proto",
                    "kanz-schemas/proto/common/v1/decimal.proto", "kanz/go.mod", "kanz/go.sum",
                    "kanz/Makefile", "kanz-schemas/buf.yaml", "kanz-schemas/buf.gen.yaml",
                    "kanz-schemas/bootstrap-go-sdk.sh", ".github/actions/setup-buf/action.yml",
                    ".github/scripts/catalog_registry_auth.py", ".github/scripts/test_catalog_registry_auth.py")
        def validate(document):
            for event in ("pull_request", "push"):
                patterns = document["on"][event]["paths"]
                if any(not any(fnmatch.fnmatchcase(source, pattern) for pattern in patterns) for source in consumed):
                    raise ValueError("consumed qualification input is unselected")
                if any(any(fnmatch.fnmatchcase(source, pattern) for pattern in patterns) for source in ("README.md", "tools/grafana_proof/proof.py")):
                    raise ValueError("unrelated source unnecessarily selects catalog runtime")
        validate(workflow)
        for event in ("pull_request", "push"):
            for source in (consumed[0], consumed[1], "kanz-schemas/proto/envelope/v1/envelope.proto"):
                changed = copy.deepcopy(workflow)
                changed["on"][event]["paths"] = [pattern for pattern in changed["on"][event]["paths"] if not fnmatch.fnmatchcase(source, pattern)]
                with self.subTest(event=event, source=source), self.assertRaises(ValueError):
                    validate(changed)

    def test_independent_source_and_clean_tree_are_required(self):
        expected = {key: value for key, value in IDENTITY.items() if key != "treeSHA"}
        run = mock.Mock(side_effect=[IDENTITY["sourceSHA"], IDENTITY["treeSHA"], ""])
        self.assertEqual(qualify.source_identity(run, expected), IDENTITY)
        for head, tree, dirty in (("c" * 40, "b" * 40, ""), ("a" * 40, "b" * 40, " M projection.py"), ("a" * 40, "main", "")):
            with self.subTest(head=head, tree=tree, dirty=dirty), self.assertRaises(ValueError):
                qualify.source_identity(mock.Mock(side_effect=[head, tree, dirty]), expected)

    def test_owner_creation_is_exclusive_and_does_not_follow_links(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owner.json"
            qualify.write_owner(path, OWNER)
            self.assertEqual(qualify.read_owner(path), OWNER)
            with self.assertRaises(FileExistsError):
                qualify.write_owner(path, OWNER)
            link = Path(directory) / "link.json"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                qualify.read_owner(link)

    def test_stale_or_foreign_owner_cannot_authorize_docker(self):
        for key, value in (("sourceSHA", "c" * 40), ("treeSHA", "c" * 40), ("runID", "999"), ("runAttempt", "2"), ("nonce", "fedcba9876543210")):
            owner = dict(OWNER, **{key: value})
            execute = mock.Mock()
            with self.subTest(key=key), mock.patch.object(qualify, "read_owner", return_value=owner), self.assertRaises(ValueError):
                qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
            execute.assert_not_called()

    def test_failed_start_still_removes_the_created_owned_container_by_id(self):
        state = {}
        deleted = []
        name = OWNER["network"] + "-gms"
        container_id = "1" * 64
        def execute(args, **_kwargs):
            if args[:3] == ["docker", "run", "--detach"]:
                state[name] = {"Id": container_id, "Config": {"Labels": qualify.labels(OWNER)}}
                raise ValueError("start failed after create")
            if args[:3] == ["docker", "ps", "--all"]:
                if "name=^/" + name + "$" in args and name in state:
                    return name
                return ""
            if args[:3] == ["docker", "inspect", "--type"]:
                return json.dumps([state[args[-1]]])
            if args[:3] == ["docker", "rm", "--force"]:
                self.assertEqual(args[-1], container_id)
                deleted.append(args[-1])
                state.clear()
                return ""
            if args[:3] in (["docker", "network", "ls"], ["docker", "image", "ls"]):
                return ""
            raise AssertionError("unexpected cleanup command")
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network, fixture.images = OWNER, OWNER["network"], {"gms": "dummy-pinned-image"}
        fixture.run = execute
        with self.assertRaises(ValueError):
            fixture.start("gms", [])
        self.assertIn(name, state)
        with mock.patch.object(qualify, "read_owner", return_value=OWNER):
            qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
        self.assertFalse(state)
        self.assertEqual(deleted, [container_id])

    def test_foreign_name_collision_is_never_deleted(self):
        name = OWNER["network"] + "-gms"
        calls = []
        def execute(args, **_kwargs):
            calls.append(args)
            if args[:3] == ["docker", "ps", "--all"]:
                return name if "name=^/" + name + "$" in args else ""
            if args[:3] == ["docker", "inspect", "--type"]:
                return json.dumps([{"Id": "1" * 64, "Config": {"Labels": {"kanz.catalog.owner": "foreign"}}}])
            if args[:3] in (["docker", "network", "ls"], ["docker", "image", "ls"]):
                return ""
            raise AssertionError("foreign container must not authorize mutation")
        with mock.patch.object(qualify, "read_owner", return_value=OWNER), self.assertRaises(ValueError):
            qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
        self.assertFalse(any(args[:2] == ["docker", "rm"] for args in calls))

    def test_partial_network_create_is_cleaned_using_the_same_owner(self):
        existing = True
        deleted = []
        network_id = "2" * 64
        def execute(args, **_kwargs):
            nonlocal existing
            if args[:3] == ["docker", "ps", "--all"]:
                return ""
            if args[:3] == ["docker", "network", "ls"]:
                return OWNER["network"] if existing else ""
            if args[:3] == ["docker", "network", "inspect"]:
                return json.dumps([{"Id": network_id, "Labels": qualify.labels(OWNER)}])
            if args[:3] == ["docker", "network", "rm"]:
                deleted.append(args[-1])
                existing = False
                return ""
            if args[:3] == ["docker", "image", "ls"]:
                return ""
            raise AssertionError("unexpected cleanup command")
        with mock.patch.object(qualify, "read_owner", return_value=OWNER):
            qualify.cleanup_owned(OWNER_PATH, IDENTITY, execute)
        self.assertEqual(deleted, [network_id])
        self.assertFalse(existing)

    def test_secret_argument_timeout_has_only_a_closed_diagnostic(self):
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "system-update", {}
        marker = "one-run-credential-marker"
        command = ["docker", "run", "--env", "PASSWORD=" + marker]
        timeout = subprocess.TimeoutExpired(command, 1)
        with mock.patch.object(qualify.subprocess, "Popen", side_effect=timeout):
            try:
                fixture.run(command)
            except ValueError as error:
                diagnostic = str(error) + traceback.format_exc()
            else:
                self.fail("timeout must refuse")
        self.assertIn("system-update", diagnostic)
        self.assertNotIn(marker, diagnostic)
        self.assertNotIn("PASSWORD=", diagnostic)
        self.assertNotIn(str(command), diagnostic)

    def test_output_overflow_is_bounded_and_terminates_the_process_group(self):
        output = bytearray(b"a" * qualify.OUTPUT_LIMIT)
        with self.assertRaises(qualify.CommandFailure):
            qualify.append_output(output, b"x")
        self.assertEqual(len(output), qualify.OUTPUT_LIMIT)
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "bounded-command", {}
        for category in ("output-budget", "timeout"):
            for parent_code in (None, 0, 7):
                for stop_failure in (False, True):
                    process = mock.Mock()
                    process.poll.return_value = parent_code
                    failure = qualify.CommandFailure("reap", "reap-timeout") if stop_failure else None
                    with self.subTest(category=category, parent_code=parent_code, stop_failure=stop_failure), mock.patch.object(qualify.subprocess, "Popen", return_value=process) as launch, mock.patch.object(qualify, "capture_process", side_effect=qualify.CommandFailure("capture", category)), mock.patch.object(qualify, "stop_process", side_effect=failure) as stop:
                        with self.assertRaises(qualify.CommandFailure) as refused:
                            fixture.run(["dummy-command"])
                        self.assertEqual(refused.exception.category, "reap-timeout" if stop_failure else category)
                        stop.assert_called_once_with(process)
                        process.stdout.close.assert_called_once_with()
                        self.assertEqual(launch.call_args.kwargs["stdout"], subprocess.PIPE)
                        self.assertTrue(launch.call_args.kwargs["start_new_session"])
        process = mock.Mock(pid=1234)
        with mock.patch.object(qualify.os, "killpg") as terminate:
            qualify.stop_process(process)
        terminate.assert_called_once_with(1234, qualify.signal.SIGKILL)
        process.wait.assert_called_once_with(timeout=10)

    def test_scanner_profile_preserves_finite_default_command_limits(self):
        self.assertEqual(qualify.command_limits(["trivy"], True), 8 << 30)
        self.assertEqual(qualify.command_limits(["docker"], False), 8 << 20)
        with self.assertRaises(ValueError):
            qualify.command_limits(["docker"], True)
        self.assertEqual(qualify.SCANNER_DISK_LIMIT, 12 << 30)
        for layer in qualify.REFERENCE_DB_LAYERS:
            self.assertGreater(layer["compressedBytes"], qualify.DEFAULT_FILE_LIMIT)
            self.assertLess(layer["compressedBytes"], qualify.SCANNER_FILE_LIMIT)
        fixture = object.__new__(qualify.Fixture)
        fixture.stage, fixture.environment = "resource-profile", {}
        for args, scanner, expected in ((["trivy"], True, 8 << 30), (["docker"], False, 8 << 20)):
            process = mock.Mock()
            process.poll.return_value = 0
            with mock.patch.object(qualify.subprocess, "Popen", return_value=process) as launch, mock.patch.object(qualify, "capture_process", return_value=(0, b"")), mock.patch.object(qualify.resource, "setrlimit") as limits:
                fixture.run(args, scanner=scanner)
                launch.call_args.kwargs["preexec_fn"]()
                limits.assert_any_call(qualify.resource.RLIMIT_FSIZE, (expected, expected))

    def test_scanner_failure_categories_keep_refusals_distinct(self):
        cases = ((b"copy error: file too large", 1, "scanner-file-budget"),
                 (b"no space left on device", 1, "scanner-disk-budget"),
                 (b"dial tcp: no such host", 1, "scanner-network"),
                 (b"Java DB download failed", 1, "scanner-java-db-download"),
                 (b"vulnerability database download failed", 1, "scanner-db-download"),
                 (b"unknown refusal", 1, "scanner-command-exit"))
        for body, code, expected in cases:
            with self.subTest(expected=expected):
                self.assertEqual(qualify.failure_category(body, code, True), expected)

    def test_scanner_cleanup_requires_exact_owned_mount_identity(self):
        mount = {"filesystem": "tmpfs", "source": OWNER["scannerMount"]["source"], "options": ["rw", "nosuid", "nodev", "noexec"]}
        execute = mock.Mock()
        with mock.patch.object(qualify, "mount_record", side_effect=[mount, None]), mock.patch.object(qualify.Path, "stat", return_value=mock.Mock(st_uid=os.getuid(), st_gid=os.getgid())), mock.patch.object(qualify.os, "statvfs", return_value=mock.Mock(f_frsize=4096, f_blocks=qualify.SCANNER_DISK_LIMIT // 4096)):
            qualify.cleanup_scanner(OWNER, OWNER_PATH, execute)
        execute.assert_called_once_with(["sudo", "-n", "umount", "--", OWNER["scannerMount"]["path"]])
        execute.reset_mock()
        with mock.patch.object(qualify, "mount_record", return_value=dict(mount, source="foreign")), self.assertRaises(ValueError):
            qualify.cleanup_scanner(OWNER, OWNER_PATH, execute)
        execute.assert_not_called()
        changed = copy.deepcopy(OWNER)
        changed["scannerMount"]["path"] = "/foreign/path"
        with self.assertRaises(ValueError):
            qualify.cleanup_scanner(changed, OWNER_PATH, execute)
        execute.assert_not_called()

    def test_receipt_cannot_pass_with_missing_cases_or_foreign_revision(self):
        selected = qualify.image_identity.selections()["gms"]
        images = {"gms": selected["reference"]}
        image_id = selected["configDigest"]
        identity = qualify.image_identity.verify_engine({"Id": image_id, "Os": selected["os"], "Architecture": selected["architecture"], "RootFS": {"Type": selected["rootFSType"], "Layers": selected["rootFSDiffIDs"]}, "RepoDigests": [selected["repository"] + "@" + selected["manifestDigest"]]}, selected)
        original = dict(IDENTITY, owner=OWNER, status="passed", cleanupPassed=True,
                        cases=list(qualify.CASES), selectedImages=images, scanFindings=[],
                        images={"gms": {"reference": images["gms"], "imageID": image_id, "identity": identity, "descriptor": dict(selected, rawManifestVerified=True, rawConfigVerified=True), "scanSHA256": "3" * 64, "scanVerdict": "passed"}},
                        dependencies={"packages": {"acryl-datahub": qualify.image_patch.lock()["cliPackageVersion"]}}, projectionTests={"count": 14},
                        qualificationControls={"count": 38}, requiredTests=sorted(qualify.REQUIRED_TESTS),
                        testsSHA256="4" * 64, projectionSHA256="5" * 64, testBinarySHA256="6" * 64)
        original["scanner"] = {"scanner": "trivy-0.72.0", "fileLimitBytes": qualify.SCANNER_FILE_LIMIT, "cacheTempLimitBytes": qualify.SCANNER_DISK_LIMIT, "stdoutLimitBytes": qualify.OUTPUT_LIMIT, "defaultFileLimitBytes": qualify.DEFAULT_FILE_LIMIT, "commandTimeoutSeconds": 600, "releasedBeforeRuntime": True, "advisoryRoles": [], "advisoryExitCode": qualify.image_patch.ADVISORY_EXIT, "referenceObservedLayers": qualify.REFERENCE_DB_LAYERS,
                               "databases": {name: {"metadata": {"Version": version}, "metadataSHA256": "a" * 64, "database": {"bytes": 100, "sha256": "b" * 64}} for name, version in (("db", 2), ("java-db", 1))}}
        original["registryAuth"] = {"verifiedBeforeTargets": True, "helperSHA256": OWNER["registryHelperSHA256"], "controlsSHA256": OWNER["registryControlsSHA256"], "controlsCount": 7}
        original["buildDiagnosticControls"] = {"count": 1, "sha256": OWNER["buildDiagnosticControlsSHA256"]}
        cleanup = {"ownershipVerified": True, "fileBackend": True, "credentialsAbsent": True, "directoryRemoved": True, "passed": True, "logout": {"attempted": True, "exitCode": 0}}
        original.update(acceptanceComplete=True, registryCleanup=dict(IDENTITY, sourceVerified=True, profile=OWNER["registryProfile"], cleanup=cleanup))
        qualify.validate_receipt(original, IDENTITY, images)
        for key, value in (("cases", list(qualify.CASES[:-1])), ("sourceSHA", "c" * 40), ("cleanupPassed", False), ("scanFindings", [{"id": "refusal"}]), ("images", {}), ("scanner", {}), ("failure", {"category": "refused"}), ("acceptanceComplete", False), ("registryAuth", {}), ("registryCleanup", {}), ("registryCleanup", dict(original["registryCleanup"], cleanup=dict(cleanup, credentialsAbsent=False))), ("dependencies", {"packages": {"acryl-datahub": "1.8.0"}})):
            report = copy.deepcopy(original)
            report[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                qualify.validate_receipt(report, IDENTITY, images)

    def test_registry_profile_is_host_only_and_never_enters_target_arguments(self):
        root = OWNER_PATH.parent
        profile = Path(OWNER["registryProfile"])
        scratch = root / "catalog-fixture"
        forbidden = (scratch, Path(OWNER["derivedImage"]["context"]), qualify.HERE, root / "catalog.test")
        helper = mock.Mock()
        with mock.patch.object(qualify, "load_registry_auth", return_value=helper):
            self.assertEqual(qualify.registry_profile(root, profile, IDENTITY, forbidden), profile)
            helper.verify_backend.assert_called_once_with(root, profile, {"source": IDENTITY["sourceSHA"], "run": "123", "attempt": "1"}, authenticated=True)
            helper.reset_mock()
            for changed_root, changed_profile, changed_forbidden in ((root, profile, (root,)), (root, scratch / profile.name, forbidden), (root, profile.with_name("foreign"), forbidden)):
                with self.assertRaises(ValueError):
                    qualify.registry_profile(changed_root, changed_profile, IDENTITY, changed_forbidden)
            helper.verify_backend.assert_not_called()
        fixture = object.__new__(qualify.Fixture)
        fixture.owner, fixture.network, fixture.scratch = OWNER, OWNER["network"], scratch
        fixture.environment = {"PATH": "/public/bin", "HOME": "/owned/host-home", "DOCKER_CONFIG": str(profile)}
        fixture.auth, fixture.images, fixture.run = "one-run-catalog-marker", {"ingestion": "sha256:" + "9" * 64}, mock.Mock(return_value="")
        fixture.cli(["python", "--version"], network=False)
        args = fixture.run.call_args.args[0]
        self.assertEqual(fixture.run.call_args.kwargs["environment"]["DOCKER_CONFIG"], str(profile))
        self.assertNotIn(str(profile), " ".join(args))
        self.assertFalse(any("DOCKER_CONFIG" in value or "GITHUB_TOKEN" in value for value in args))

    def test_workflow_authentication_follows_host_builds_and_always_closes_profile(self):
        path = Path(os.environ.get("KANZ_CATALOG_WORKFLOW", qualify.ROOT / ".github/workflows/catalog-verification.yml"))
        workflow = yaml.load(path.read_text(), Loader=yaml.BaseLoader)
        def validate(document):
            steps = document["jobs"]["actual-catalog"]["steps"]
            profile = next(index for index, step in enumerate(steps) if step.get("id") == "registry-profile")
            login = next(index for index, step in enumerate(steps) if step.get("uses", "").startswith("docker/login-action@"))
            proof = next(index for index, step in enumerate(steps) if step.get("id") == "catalog-proof")
            target_cleanup = next(index for index, step in enumerate(steps) if step.get("id") == "owned-cleanup")
            registry_cleanup = next(index for index, step in enumerate(steps) if step.get("id") == "registry-cleanup")
            if document["permissions"].get("packages") != "read" or not profile < login < proof < target_cleanup < registry_cleanup or "if" in steps[login] or steps[login]["with"].get("logout") != "false" or steps[target_cleanup].get("if") != "always()" or steps[registry_cleanup].get("if") != "always() && steps.registry-profile.outcome == 'success'" or "--registry-cleanup" not in steps[registry_cleanup]["run"]:
                raise ValueError("registry lifecycle workflow contract differs")
            before = "\n".join(step.get("run", "") for step in steps[:profile])
            after = "\n".join(step.get("run", "") for step in steps[profile:])
            required = ("go test -c", "make -C kanz generate", "install-audit.sh", "test_catalog_registry_auth.py", "test_build_diagnostics.py", "--verify-source", "EveryDockerfileBaseImageComesFromTheMirror", "WorkflowsBuildingMirrorImagesAuthenticateFirst")
            if any(value not in before for value in required) or any(value in after for value in ("go test -c", "make -C kanz generate", "install-audit.sh")):
                raise ValueError("host build or required guard crosses credential introduction")
        validate(workflow)
        for mutation in ("permission", "logout", "cleanup", "ordering", "diagnostics", "guard"):
            changed = copy.deepcopy(workflow)
            steps = changed["jobs"]["actual-catalog"]["steps"]
            if mutation == "permission":
                changed["permissions"]["packages"] = "write"
            elif mutation == "logout":
                next(step for step in steps if step.get("uses", "").startswith("docker/login-action@"))["with"]["logout"] = "true"
            elif mutation == "cleanup":
                next(step for step in steps if step.get("id") == "registry-cleanup")["if"] = "success()"
            elif mutation == "ordering":
                selected = next(step for step in steps if step.get("id") == "registry-profile")
                steps.remove(selected)
                steps.insert(0, selected)
            elif mutation == "diagnostics":
                selected = next(step for step in steps if "test_build_diagnostics.py" in step.get("run", ""))
                steps.remove(selected)
                steps.append(selected)
            else:
                for step in steps:
                    step["run"] = step.get("run", "").replace("EveryDockerfileBaseImageComesFromTheMirror", "MissingGuard")
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                validate(changed)


if __name__ == "__main__":
    unittest.main()
