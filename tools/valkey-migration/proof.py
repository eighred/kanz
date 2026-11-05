"""Disposable retained-volume migration; accepts no endpoint or live target."""
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
import uuid

REDIS = "redis@sha256:bb186d083732f669da90be8b0f975a37812b15e913465bb14d845db72a4e3e08"
VALKEY = "valkey/valkey:9.1.3-alpine@sha256:bf09d58e96d6b4524a15396b65b242bbe18beebbcc15a04d8ee75ddac62fae0b"
REDIS_VERSION = "7.4.5"
LABEL = "io.kanz.valkey-migration"
KEYS = {"tenant-a:nonce:retained": "claimed", "tenant-b:nonce:retained": "independent",
        "tenant-a:idempotency": 'response:<>&\u2028\n"exact"', "tenant-a:counter": "19"}
CASES = ["exact_values", "absolute_expiry", "tenant_distinct_keys", "retained_claim_refusal",
         "valkey_written_claim", "counter_update", "valkey_restart", "redis_rollback", "expiry_after_rollback", "authentication"]


class Refused(RuntimeError):
    pass


def command(args, data=None, timeout=60):
    try:
        result = subprocess.run(args, input=data, capture_output=True, text=True, encoding="utf-8", timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        raise Refused("command_unavailable_or_timeout") from None
    if result.returncode:
        raise Refused(command_failure(args, result.stderr))
    return result.stdout.strip()


def validate_name(name):
    if not re.fullmatch(r"kanz-valkey-proof-[0-9a-f]{24}", name):
        raise Refused("invalid_fixture_name")


def command_failure(args, diagnostic):
    operation = args[1] if args[:1] == ["docker"] and len(args) > 1 and args[1] in {
        "run", "exec", "inspect", "stop", "rm", "volume", "image", "ps"} else "other"
    text = diagnostic.lower()
    for marker, category in (("toomanyrequests", "registry_rate_limit"),
                             ("failed to resolve reference", "registry_resolution_failed"),
                             ("pull access denied", "image_pull_refused"),
                             ("unknown log opt", "logging_option_refused"),
                             ("failed to initialize logging driver", "logging_driver_refused"),
                             ("no space left on device", "runner_storage_full"),
                             ("permission denied", "permission_refused"),
                             ("cannot connect to the docker daemon", "daemon_unavailable"),
                             ("oci runtime create failed", "container_create_failed"),
                             ("failed to create task", "container_task_failed"),
                             ("exec format error", "binary_format_refused")):
        if marker in text:
            return operation + ":" + category
    return operation + ":unclassified_command_failure"


def config(password, preamble):
    if not re.fullmatch(r"[0-9a-f]{48}", password) or type(preamble) is not bool:
        raise Refused("invalid_fixture_config")
    return ("bind 127.0.0.1\nport 6379\ndir /data\nappendonly yes\nappendfsync always\n"
            "maxmemory 64mb\nmaxmemory-policy noeviction\nsave \"\"\n"
            "aof-use-rdb-preamble " + ("yes" if preamble else "no") + "\nrequirepass " + password + "\n")


def info(text):
    return dict(line.split(":", 1) for line in text.splitlines() if line and not line.startswith("#") and ":" in line)


def startup_category(log):
    # Only fixed classifications leave this function, never raw server logs.
    for marker, category in (("Can't handle RDB format version", "unsupported_rdb_version"),
                             ("Wrong signature trying to load DB", "unsupported_rdb_signature"),
                             ("Permission denied", "filesystem_permission_refused"),
                             ("Bad file format reading the append only file", "aof_format_refused"),
                             ("Address already in use", "bind_refused")):
        if marker in log:
            return category
    return "unclassified_startup_refusal"


def verify_snapshot(actual, expected):
    if actual != expected:
        raise Refused("state_or_absolute_expiry_changed")


def validate_report(report, source):
    phases = {"redis": REDIS, "valkey": VALKEY, "valkey_restart": VALKEY, "redis_rollback": REDIS}
    if (not re.fullmatch(r"[0-9a-f]{40}", source) or report.get("source") != source
            or report.get("verdict") != "PASS" or report.get("cleanup") != "verified_absent"
            or report.get("cases") != CASES or set(report.get("phases", {})) != set(phases)):
        raise Refused("incomplete_migration_evidence")
    for name, image in phases.items():
        observed = report["phases"][name]
        if (observed.get("image") != image or not re.fullmatch(r"sha256:[0-9a-f]{64}", observed.get("imageID", ""))
                or (image == VALKEY and observed.get("version") != "9.1.3")
                or (image == REDIS and observed.get("version") != REDIS_VERSION)):
            raise Refused("unexpected_runtime_identity")
    if (report["phases"]["redis"] != report["phases"]["redis_rollback"]
            or report["phases"]["valkey"] != report["phases"]["valkey_restart"]):
        raise Refused("runtime_identity_changed")


class Fixture:
    def __init__(self, name):
        validate_name(name)
        self.name, self.volume = name, name + "-data"
        self.password = secrets.token_hex(24)
        self.cli = "redis-cli"
        self.image = REDIS

    def owned(self):
        item = json.loads(command(["docker", "inspect", self.name]))[0]
        if (item["Config"].get("Labels", {}).get(LABEL) != self.name
                or item["HostConfig"]["NetworkMode"] != "none"
                or item["Config"]["Image"] != self.image
                or item["Image"] != command(["docker", "image", "inspect", self.image, "--format", "{{.Id}}"])):
            raise Refused("container_identity_mismatch")
        mounts = item.get("Mounts", [])
        if len(mounts) != 1 or mounts[0].get("Name") != self.volume or mounts[0].get("Destination") != "/data":
            raise Refused("unexpected_fixture_mount")
        return item["Image"]

    def volume_owned(self):
        item = json.loads(command(["docker", "volume", "inspect", self.volume]))[0]
        if item.get("Labels", {}).get(LABEL) != self.name:
            raise Refused("volume_identity_mismatch")

    def base(self, image):
        return ["docker", "run", "--name", self.name, "--label", LABEL + "=" + self.name,
                "--network", "none", "--user", "999:999", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "--read-only", "--memory=192m", "--cpus=1",
                "--pids-limit=64", "--log-driver=local", "--log-opt=max-size=1m", "--log-opt=max-file=1",
                "--mount", "type=volume,src=" + self.volume + ",dst=/data"]

    def start(self, image, preamble):
        self.volume_owned()
        self.image = image
        self.cli = "valkey-cli" if image == VALKEY else "redis-cli"
        server = "valkey-server" if image == VALKEY else "redis-server"
        # No host mounts or host ports. The same private volume crosses all phases.
        command(self.base(image) + ["--rm", "-i", image, "sh", "-c", "cat > /data/proof.conf"], config(self.password, preamble), timeout=180)
        command(self.base(image) + ["-d", image, server, "/data/proof.conf"], timeout=180)
        loaded = self.owned()
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                if self.call("PING") == "PONG":
                    break
            except (Refused, ValueError):
                pass
            time.sleep(.2)
        else:
            # Docker logs are bounded and stay inside this disposable runner;
            # fixture credentials/config and free text never enter the artifact.
            try:
                raw = subprocess.run(["docker", "logs", "--tail", "80", self.name], capture_output=True,
                                     text=True, encoding="utf-8", timeout=10)
                category = startup_category(raw.stdout + raw.stderr) if raw.returncode == 0 else "startup_diagnostic_unavailable"
            except (OSError, subprocess.TimeoutExpired):
                category = "startup_diagnostic_unavailable"
            raise Refused(category)
        server_info = info(self.raw("INFO", "server"))
        version = server_info.get("valkey_version") if image == VALKEY else server_info.get("redis_version")
        if (image == VALKEY and version != "9.1.3") or (image == REDIS and version != REDIS_VERSION):
            raise Refused("unexpected_server_version")
        for key, value in {"appendonly": "yes", "appendfsync": "always", "maxmemory-policy": "noeviction", "maxmemory": "67108864",
                           "aof-use-rdb-preamble": "yes" if preamble else "no"}.items():
            result = self.call("CONFIG", "GET", key)
            if result not in ([key, value], {key: value}):
                raise Refused("persistence_configuration_mismatch")
        unauth = command(["docker", "exec", self.name, self.cli, "--raw", "PING"])
        if not unauth.startswith("NOAUTH "):
            raise Refused("unauthenticated_connection_not_refused")
        return {"image": image, "imageID": loaded, "version": version}

    def raw(self, *args):
        return command(["docker", "exec", "-e", "REDISCLI_AUTH=" + self.password, self.name, self.cli, "--raw", *map(str, args)])

    def call(self, *args):
        return json.loads(command(["docker", "exec", "-e", "REDISCLI_AUTH=" + self.password, self.name, self.cli, "--json", *map(str, args)]))

    def now(self):
        seconds, micros = self.call("TIME")
        return int(seconds) * 1000 + int(micros) // 1000

    def snapshot(self, keys):
        return {key: {"value": self.call("GET", key), "expires": self.call("PEXPIRETIME", key)} for key in keys}

    def claim_refused(self, key, expires):
        if self.call("SET", key, "replayed", "PXAT", expires, "NX") is not None:
            raise Refused("retained_claim_reopened")

    def rewrite(self, preamble):
        self.call("BGREWRITEAOF")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            state = info(self.raw("INFO", "persistence"))
            if state.get("aof_rewrite_in_progress") == "0" and state.get("aof_last_bgrewrite_status") == "ok":
                break
            time.sleep(.2)
        else:
            raise Refused("aof_rewrite_not_complete")
        manifest = command(["docker", "exec", self.name, "cat", "/data/appendonlydir/appendonly.aof.manifest"])
        base = [line.split()[1] for line in manifest.splitlines() if line.endswith("type b")]
        if len(base) != 1 or not re.fullmatch(r"appendonly\.aof\.[0-9]+\.base\." + ("rdb" if preamble else "aof"), base[0]):
            raise Refused("unexpected_aof_base_format")

    def stop(self):
        self.owned()
        command(["docker", "stop", "--time", "30", self.name])
        item = json.loads(command(["docker", "inspect", self.name]))[0]
        if item["State"]["ExitCode"] != 0 or item["State"].get("OOMKilled"):
            raise Refused("unclean_server_stop")
        command(["docker", "rm", self.name])

    def cleanup(self):
        if self.name in command(["docker", "ps", "-a", "--format", "{{.Names}}"]).splitlines():
            self.owned()
            command(["docker", "rm", "-f", self.name])
        if self.volume in command(["docker", "volume", "ls", "--format", "{{.Name}}"]).splitlines():
            self.volume_owned()
            command(["docker", "volume", "rm", self.volume])
        if (self.name in command(["docker", "ps", "-a", "--format", "{{.Names}}"]).splitlines()
                or self.volume in command(["docker", "volume", "ls", "--format", "{{.Name}}"]).splitlines()):
            raise Refused("cleanup_not_verified")


def main():
    fixture = Fixture("kanz-valkey-proof-" + uuid.uuid4().hex[:24])
    report = {"source": command(["git", "rev-parse", "HEAD"]), "verdict": "FAIL", "phases": {}, "cleanup": "unknown"}
    stage = "create"
    try:
        command(["docker", "volume", "create", "--label", LABEL + "=" + fixture.name, fixture.volume])
        stage = "redis_seed"
        report["phases"]["redis"] = fixture.start(REDIS, True)
        expires = fixture.now() + 900_000
        for key, value in KEYS.items():
            if fixture.call("SET", key, value, "PXAT", expires, "NX") != "OK":
                raise Refused("seed_failed")
        expected = fixture.snapshot(KEYS)
        verify_snapshot(expected, {key: {"value": value, "expires": expires} for key, value in KEYS.items()})
        fixture.rewrite(True)
        fixture.stop()
        stage = "valkey_upgrade"
        report["phases"]["valkey"] = fixture.start(VALKEY, False)
        verify_snapshot(fixture.snapshot(KEYS), expected)
        fixture.claim_refused("tenant-a:nonce:retained", expires)
        new_key = "tenant-a:nonce:valkey-written"
        if fixture.call("SET", new_key, "new-claim", "PXAT", expires, "NX") != "OK":
            raise Refused("new_claim_failed")
        if fixture.call("INCR", "tenant-a:counter") != 20:
            raise Refused("counter_changed")
        expected[new_key] = {"value": "new-claim", "expires": expires}
        expected["tenant-a:counter"]["value"] = "20"
        verify_snapshot(fixture.snapshot(expected), expected)
        fixture.rewrite(False)
        fixture.stop()
        stage = "valkey_restart"
        report["phases"]["valkey_restart"] = fixture.start(VALKEY, False)
        verify_snapshot(fixture.snapshot(expected), expected)
        fixture.claim_refused(new_key, expires)
        fixture.stop()
        stage = "redis_rollback"
        report["phases"]["redis_rollback"] = fixture.start(REDIS, False)
        verify_snapshot(fixture.snapshot(expected), expected)
        for key in ("tenant-a:nonce:retained", new_key):
            fixture.claim_refused(key, expires)
        # Absolute expiry still operates after rollback; no renewed TTL window.
        expiry_key = "tenant-a:nonce:expires"
        deadline_ms = fixture.now() + 300
        fixture.call("SET", expiry_key, "short", "PXAT", deadline_ms)
        deadline = time.monotonic() + 5
        while fixture.call("GET", expiry_key) is not None and time.monotonic() < deadline:
            time.sleep(.1)
        if fixture.call("GET", expiry_key) is not None or fixture.call("PEXPIRETIME", expiry_key) != -2:
            raise Refused("expiry_failed")
        if fixture.call("DBSIZE") != len(expected):
            raise Refused("unexpected_key_inventory")
        fixture.stop()
        report["cases"] = CASES
        report["verdict"] = "PASS"
    except (Refused, ValueError, KeyError, TypeError) as error:
        report["failedStage"] = stage
        report["reason"] = str(error) if isinstance(error, Refused) else "fixture_decode_refused"
    finally:
        try:
            fixture.cleanup()
            report["cleanup"] = "verified_absent"
        except (Refused, ValueError, KeyError, TypeError):
            report["cleanup"] = "unknown"
            report["verdict"] = "FAIL"
        if report["verdict"] == "PASS":
            try:
                validate_report(report, report["source"])
            except (Refused, ValueError, KeyError, TypeError):
                report["verdict"] = "FAIL"
                report["reason"] = "incomplete_migration_evidence"
        Path(os.environ["RUNNER_TEMP"], "valkey-migration.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report))
    return 0 if report["verdict"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
