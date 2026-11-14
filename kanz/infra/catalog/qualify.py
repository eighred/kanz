"""Disposable Linux catalog proof. Pull exact inputs before internal networking.

No production endpoint/configuration is accepted. Containers receive no host
credentials/socket; all listeners stay inside one internal network. Only the
CLI sees read-only catalog source and its assigned scratch directory.
"""
import argparse
import hashlib
import json
import os
import re
import resource
import secrets
import subprocess
import sys
import tempfile
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
PIN = re.compile(r"[a-z0-9./_-]+:[A-Za-z0-9_.-]+@sha256:[0-9a-f]{64}\Z")


class Fixture:
    def __init__(self, scratch, binary):
        self.scratch = scratch
        self.binary = binary
        self.network = "kanz-catalog-" + secrets.token_hex(8)
        self.network_created = False
        self.containers = []
        self.environment = {"PATH": os.environ["PATH"], "HOME": str(scratch / "host-home"), "DOCKER_CONFIG": str(scratch / "docker-config"), "LANG": "C.UTF-8"}
        for path in ("host-home", "docker-config", "home"):
            (scratch / path).mkdir()
        self.secret = secrets.token_hex(32)
        self.auth = "Basic __datahub_system:" + self.secret
        self.lock = json.loads((HERE / "images.json").read_text())
        self.images = self.lock["images"]
        if self.lock["platform"] != "linux/amd64" or set(self.images) != {"gms", "upgrade", "ingestion", "mysql", "search", "broker"} or not all(PIN.fullmatch(v) for v in self.images.values()):
            raise ValueError("closed image inventory differs")
        self.stage = "initialization"

    def run(self, args, timeout=180, environment=None):
        def limits():
            resource.setrlimit(resource.RLIMIT_FSIZE, (8 * 1024 * 1024, 8 * 1024 * 1024))
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(args, stdout=output, stderr=subprocess.STDOUT, env=environment or self.environment, timeout=timeout, check=False, preexec_fn=limits)
            if result.returncode or output.tell() > 8 * 1024 * 1024:
                # Never print command arguments, environment or runtime logs:
                # one-run credentials are still credentials.
                raise ValueError("catalog operation refused at " + self.stage)
            output.seek(0)
            return output.read().decode()

    def start(self, role, options, command=(), memory="1g", entrypoint=None):
        name = self.network + "-" + role
        args = ["docker", "run", "--detach", "--name", name, "--network", self.network,
                "--network-alias", role, "--platform", "linux/amd64", "--read-only",
                "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256",
                "--cpus", "2", "--memory", memory, "--memory-swap", memory,
                "--ulimit", "nofile=65536:65536", "--log-opt", "max-size=1m", "--log-opt", "max-file=1", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m"]
        if entrypoint:
            args += ["--entrypoint", entrypoint]
        args += options + [self.images[role]] + list(command)
        self.run(args)
        self.containers.append(name)
        return name

    def cli(self, command, metadata="metadata.json", network=True):
        child = dict(self.environment, DATAHUB_GMS_AUTHORIZATION=self.auth)
        args = ["docker", "run", "--rm", "--platform", "linux/amd64", "--network", self.network if network else "none",
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
                "--memory", "1g", "--memory-swap", "1g", "--cpus", "2", "--user", f"{os.getuid()}:{os.getgid()}",
                "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m", "--mount", f"type=bind,src={HERE},dst=/catalog,readonly",
                "--mount", f"type=bind,src={self.scratch},dst=/scratch", "--workdir", "/scratch",
                "--env", "HOME=/scratch/home", "--env", "PYTHONDONTWRITEBYTECODE=1", "--env", "DATAHUB_TELEMETRY_ENABLED=false",
                "--env", "DATAHUB_GMS_URL=http://gms:8080", "--env", "DATAHUB_GMS_AUTHORIZATION",
                "--env", "KANZ_CATALOG_METADATA_FILE=/scratch/" + metadata,
                "--entrypoint", command[0], self.images["ingestion"], *command[1:]]
        return self.run(args, timeout=240, environment=child)

    def setup(self):
        self.stage = "immutable-inputs"
        identities = {}
        for role, image in self.images.items():
            self.run(["docker", "pull", "--platform", "linux/amd64", image], timeout=600)
            info = json.loads(self.run(["docker", "image", "inspect", image]))[0]
            if info["Os"] != "linux" or info["Architecture"] != "amd64" or not any(x.endswith("@" + image.rsplit("@", 1)[1]) for x in info.get("RepoDigests", [])):
                raise ValueError("executed image identity differs")
            identities[role] = {"reference": image, "imageID": info["Id"]}
        self.stage = "fixed-critical-image-scan"
        config = self.scratch / "trivy.yaml"
        config.write_text("{}")
        scan_options = ["--config", str(config), "--ignorefile", "/dev/null", "--cache-dir", str(self.scratch / "trivy-cache")]
        self.run(["trivy", "image", *scan_options, "--download-db-only"], timeout=600)
        for role, image in self.images.items():
            self.stage = "fixed-critical-image-scan:" + role
            output = self.scratch / ("scan-" + role + ".json")
            self.run(["trivy", "image", *scan_options, "--skip-db-update", "--scanners", "vuln", "--ignore-unfixed", "--severity", "CRITICAL", "--exit-code", "1", "--format", "json", "--output", str(output), image], timeout=600)
            identities[role]["scanSHA256"] = hashlib.sha256(output.read_bytes()).hexdigest()
        self.run(["docker", "network", "create", "--internal", self.network])
        self.network_created = True
        self.stage = "broker"
        broker = self.start("broker", ["--tmpfs", "/var/lib/redpanda/data:rw,nosuid,nodev,size=1g,uid=101,gid=101"],
                            ["redpanda", "start", "--config", "/tmp/redpanda.yaml", "--overprovisioned", "--smp", "1", "--memory", "512M", "--reserve-memory", "0M", "--node-id", "0", "--check=false", "--kafka-addr", "0.0.0.0:9092", "--advertise-kafka-addr", "broker:9092", "--set", "redpanda.auto_create_topics_enabled=true"])
        self.stage = "mysql"
        mysql = self.start("mysql", ["--user", "999:999", "--tmpfs", "/var/lib/mysql:rw,nosuid,nodev,size=1g,uid=999,gid=999", "--tmpfs", "/var/run/mysqld:rw,nosuid,nodev,size=16m,uid=999,gid=999", "--env", "MYSQL_ROOT_PASSWORD=" + self.secret, "--env", "MYSQL_ROOT_HOST=%", "--env", "MYSQL_DATABASE=datahub"], memory="1g")
        self.stage = "search"
        search = self.start("search", ["--user", "1000:1000", "--tmpfs", "/usr/share/opensearch/data:rw,nosuid,nodev,size=1g,uid=1000,gid=1000", "--tmpfs", "/usr/share/opensearch/logs:rw,nosuid,nodev,size=64m,uid=1000,gid=1000", "--env", "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m"],
                            ["-Ediscovery.type=single-node", "-Eplugins.security.disabled=true", "-Enetwork.host=0.0.0.0", "-Enode.store.allow_mmap=false"], memory="2g", entrypoint="/usr/share/opensearch/bin/opensearch")
        self.stage = "storage-readiness"
        self.wait_exec(broker, ["rpk", "cluster", "info", "-X", "brokers=broker:9092"])
        self.wait_exec(mysql, ["mysqladmin", "ping", "--silent"])
        self.wait_exec(search, ["curl", "--fail", "--silent", "http://search:9200/_cluster/health"])
        common = {
            "EBEAN_DATASOURCE_DRIVER": "com.mysql.jdbc.Driver", "EBEAN_DATASOURCE_HOST": "mysql:3306",
            "EBEAN_DATASOURCE_URL": "jdbc:mysql://mysql:3306/datahub?useSSL=false&allowPublicKeyRetrieval=true&characterEncoding=UTF-8",
            "EBEAN_DATASOURCE_USERNAME": "root", "EBEAN_DATASOURCE_PASSWORD": self.secret,
            "ELASTICSEARCH_HOST": "search", "ELASTICSEARCH_PORT": "9200", "ELASTICSEARCH_PROTOCOL": "http",
            "ELASTICSEARCH_USE_SSL": "false", "ELASTICSEARCH_IMPLEMENTATION": "opensearch", "GRAPH_SERVICE_IMPL": "elasticsearch",
            "KAFKA_BOOTSTRAP_SERVER": "broker:9092", "KAFKA_SCHEMAREGISTRY_URL": "http://gms:8080/schema-registry/api/",
            "SCHEMA_REGISTRY_TYPE": "INTERNAL", "DATAHUB_SYSTEM_CLIENT_ID": "__datahub_system",
            "DATAHUB_SYSTEM_CLIENT_SECRET": self.secret, "DATAHUB_TOKEN_SERVICE_SIGNING_KEY": secrets.token_hex(32),
            "DATAHUB_TOKEN_SERVICE_SALT": secrets.token_hex(32), "DATAHUB_TELEMETRY_ENABLED": "false",
            "ENTITY_REGISTRY_CONFIG_PATH": "/datahub/datahub-gms/resources/entity-registry.yml",
            "JAVA_OPTS": "-Xms512m -Xmx1g", "EXTRACT_JAR_ENABLED": "false", "ENTITY_VERSIONING_ENABLED": "true",
        }
        options = [item for key, value in common.items() for item in ("--env", key + "=" + value)]
        self.stage = "system-update"
        upgrade = self.start("upgrade", options + ["--env", "DATAHUB_PRECREATE_TOPICS=true", "--env", "DATAHUB_SQL_SETUP_ENABLED=true", "--env", "CREATE_USER=false", "--env", "SCHEMA_REGISTRY_SYSTEM_UPDATE=true", "--env", "PARTITIONS=1"], ["-u", "SystemUpdate"], memory="2g")
        code = self.run(["docker", "wait", upgrade], timeout=300).strip()
        if code != "0":
            raise ValueError("catalog system update refused")
        self.stage = "gms"
        gms = self.start("gms", options + ["--env", "METADATA_SERVICE_AUTH_ENABLED=true", "--env", "MAE_CONSUMER_ENABLED=true", "--env", "MCE_CONSUMER_ENABLED=true", "--env", "PE_CONSUMER_ENABLED=true", "--env", "DEV_TOOLING_ENABLED=false"], memory="2g")
        self.wait_exec(gms, ["curl", "--fail", "--silent", "http://gms:8080/health"], timeout=300)
        return identities

    def go_tests(self, name, evidence):
        self.stage = name
        command = ["docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
                   "--security-opt", "no-new-privileges", "--pids-limit", "64", "--memory", "1g", "--memory-swap", "1g", "--cpus", "2",
                   "--user", f"{os.getuid()}:{os.getgid()}", "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
                   "--mount", f"type=bind,src={self.scratch},dst=/scratch",
                   "--mount", f"type=bind,src={self.binary},dst=/catalog.test,readonly",
                   "--env", "TEST_CATALOG_SCRATCH=/scratch", "--entrypoint", "/catalog.test", self.images["ingestion"],
                   "-test.v", "-test.count=1", "-test.timeout=120s", "-test.run=" + name]
        output = self.run(["go", "tool", "test2json", "-p", "github.com/eighred/kanz/services/lineage/internal/server", "-t", *command], timeout=180)
        (self.scratch / evidence).write_text(output)

    def wait_exec(self, container, args, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                self.run(["docker", "exec", container, *args], timeout=15)
                return
            except ValueError:
                state = json.loads(self.run(["docker", "inspect", "--format", "{{json .State}}", container]))
                if not state["Running"]:
                    raise ValueError("fixture stopped at " + self.stage) from None
                time.sleep(2)
        raise ValueError("fixture readiness exceeded bound at " + self.stage)

    def close(self):
        complete = True
        for container in reversed(self.containers):
            try:
                result = subprocess.run(["docker", "rm", "--force", "--volumes", container], env=self.environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, check=False)
                complete = complete and result.returncode == 0
            except (OSError, subprocess.TimeoutExpired):
                complete = False
        if self.network_created:
            try:
                result = subprocess.run(["docker", "network", "rm", self.network], env=self.environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, check=False)
                complete = complete and result.returncode == 0
            except (OSError, subprocess.TimeoutExpired):
                complete = False
        return complete


def qualify(scratch, binary):
    fixture = Fixture(scratch, binary)
    report = {"sourceSHA": fixture.run(["git", "-C", str(ROOT), "rev-parse", "HEAD"]).strip(),
              "treeSHA": fixture.run(["git", "-C", str(ROOT), "rev-parse", "HEAD^{tree}"]).strip(),
              "runID": os.environ["GITHUB_RUN_ID"], "runAttempt": os.environ["GITHUB_RUN_ATTEMPT"],
              "status": "failed", "cases": [], "selectedImages": fixture.images,
              "testBinarySHA256": hashlib.sha256(binary.read_bytes()).hexdigest()}
    try:
        report["images"] = fixture.setup()
        fixture.stage = "locked-cli-version"
        dependencies = json.loads(fixture.cli(["python", "-c", "import importlib.metadata,json,sys; print(json.dumps({'python':sys.version.split()[0],'packages':{d.metadata['Name']:d.version for d in importlib.metadata.distributions()}}))"], network=False))
        if dependencies["packages"].get("acryl-datahub") != "1.8.0":
            raise ValueError("actual ingestion package version differs")
        report["dependencies"] = dependencies
        fixture.go_tests("^TestCatalogFixtureExport$", "export-tests.jsonl")
        fixture.stage = "projection-negative-controls"
        unit_output = fixture.cli(["python", "-B", "-m", "unittest", "discover", "-s", "/catalog", "-p", "test_projection.py", "-v"], network=False)
        if not re.search(r"Ran 13 tests in", unit_output) or "skipped" in unit_output:
            raise ValueError("complete projection negative-control inventory required")
        (scratch / "projection-tests.txt").write_text(unit_output)
        report["projectionTests"] = {"count": 13, "sha256": hashlib.sha256(unit_output.encode()).hexdigest()}
        fixture.stage = "metadata-projection"
        fixture.cli(["python", "/catalog/projection.py", "--mapping", "/catalog/fixture_mapping.json", "--events", "/scratch/events.json", "--output", "/scratch/metadata.json"], network=False)
        fixture.stage = "actual-ingestion"
        fixture.cli(["datahub", "ingest", "-c", "/catalog/datahub-recipe.yaml"])
        fixture.stage = "actual-readback"
        fixture.cli(["python", "/catalog/catalog_client.py", "readback"])
        fixture.stage = "actual-metadata-alteration"
        fixture.cli(["datahub", "ingest", "-c", "/catalog/datahub-recipe.yaml"], metadata="altered.json")
        fixture.cli(["python", "/catalog/catalog_client.py", "altered"])
        fixture.go_tests(".", "server-tests.jsonl")
        report["cases"] = json.loads((scratch / "readback.json").read_text())["cases"]
        report["projectionSHA256"] = hashlib.sha256((scratch / "metadata.json").read_bytes()).hexdigest()
        required = {"TestCatalogMetadataCannotGrantLineageAccess/" + name for name in ("anonymous-catalog", "anonymous-lineage", "catalog-owner-is-not-a-grant", "steward-still-authorized", "upstream-pii-still-redacted")}
        required.add("TestCatalogMetadataCannotGrantLineageAccess")
        required.add("TestCatalogFixtureExport")
        events = [json.loads(line) for line in (scratch / "server-tests.jsonl").read_text().splitlines()]
        if any(e.get("Action") in {"skip", "fail"} for e in events) or not required.issubset({e.get("Test") for e in events if e.get("Action") == "pass"}):
            raise ValueError("mandatory lineage cases did not pass without skips")
        report["testsSHA256"] = hashlib.sha256((scratch / "server-tests.jsonl").read_bytes()).hexdigest()
        report["requiredTests"] = sorted(required)
        report["cases"].append("lineage-authorization-after-alteration")
        report["status"] = "passed"
    finally:
        report["stage"] = fixture.stage
        report["cleanupPassed"] = fixture.close()
        if not report["cleanupPassed"]:
            report["status"] = "failed"
        report["scanFindings"] = []
        for path in scratch.glob("scan-*.json"):
            try:
                scan = json.loads(path.read_text())
            except (ValueError, OSError):
                report["status"] = "failed"
                report["scanFindings"].append({"image": fixture.images[path.stem.removeprefix("scan-")], "error": "incomplete-scanner-output"})
                continue
            for result in scan.get("Results", []):
                for item in result.get("Vulnerabilities", []):
                    report["scanFindings"].append({"image": fixture.images[path.stem.removeprefix("scan-")], "id": item["VulnerabilityID"], "package": item["PkgName"], "installed": item["InstalledVersion"], "fixed": item.get("FixedVersion", ""), "severity": item["Severity"]})
        (scratch / "catalog-receipt.json").write_text(json.dumps(report, indent=2, sort_keys=True))
        if not report["cleanupPassed"]:
            raise ValueError("disposable catalog cleanup refused")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", type=Path, required=True)
    parser.add_argument("--test-binary", type=Path, required=True)
    args = parser.parse_args()
    if sys.platform != "linux" or not args.scratch.is_dir():
        raise SystemExit("catalog qualification requires assigned Linux scratch")
    if not args.test_binary.is_file() or args.test_binary.resolve().is_relative_to(args.scratch.resolve()):
        raise SystemExit("readonly test binary must be outside assigned scratch")
    qualify(args.scratch.resolve(), args.test_binary.resolve())
