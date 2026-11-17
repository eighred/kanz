"""Run only on a disposable GitHub runner, using private loopback containers."""
from pathlib import Path
import os
import json
import re
import subprocess
import sys
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
HELPER = "tools/risk_shared_dedup_proof/Dockerfile"
SERVICE = "kanz/services/risk_engine/Dockerfile"
VALKEY = "valkey/valkey:9.1.3-alpine@sha256:bf09d58e96d6b4524a15396b65b242bbe18beebbcc15a04d8ee75ddac62fae0b"
POSTGRES = "postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9"
RUNTIME = "ghcr.io/eighred/base/distroless-static:nonroot"


def command(args, *, timeout=30, check=True, data=None):
    result = subprocess.run(args, cwd=ROOT, timeout=timeout, input=data,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if check and result.returncode:
        causes = [msg for msg in ("unauthorized", "denied", "manifest unknown", "no space left",
                  "not found", "out of memory", "unknown flag") if msg in result.stdout.lower()]
        sites = sorted(set(re.findall(r"[a-zA-Z0-9_./-]+\.go:\d+(?::\d+)?", result.stdout)))
        raise RuntimeError(f"{args[:3]} exit {result.returncode}, categories {causes}, source locations {sites[:5]}")
    return result


def docker(*args, **options):
    return command(["docker", *args], **options)


def identity(name):
    value = docker("image", "inspect", "--format", "{{.Id}}", name).stdout.strip()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", value):
        raise RuntimeError("unresolved image identity")
    return value


def build(revision):
    # The generated SDK is the only output shared with the canonical service
    # build context. All Go tools and source builds run in build containers.
    helper = ["docker", "buildx", "build", "--progress=plain", "-f", HELPER]
    sdk = ROOT / "schemas/gen/go"
    if sdk.exists():
        raise RuntimeError("source checkout already contains generated SDK")
    command([*helper, "--load", "--target", "driver", "-t", "risk-proof:driver", "."], timeout=1500)
    command([*helper, "--target", "sdk", "--output", f"type=local,dest={sdk}", "."], timeout=300)
    docker("pull", RUNTIME, timeout=120)
    runtime = docker("image", "inspect", "--format", "{{index .RepoDigests 0}}", RUNTIME).stdout.strip()
    if not re.fullmatch(r"ghcr\.io/eighred/base/distroless-static@sha256:[0-9a-f]{64}", runtime):
        raise RuntimeError("unresolved canonical runtime base")
    for target in ("shared-dedup", "default"):
        command(["docker", "buildx", "build", "--load", "--progress=plain",
                 "--build-context", RUNTIME+"=docker-image://"+runtime, "--target", target,
                 "--build-arg", "VERSION="+revision, "-f", SERVICE, "-t", "risk-proof:"+target, "."], timeout=1200)
    for name in (VALKEY, POSTGRES):
        docker("pull", name, timeout=150)
    print("BUILT", revision, runtime, *(identity("risk-proof:"+t) for t in ("shared-dedup","default","driver")), flush=True)


class Fixture:
    def __init__(self):
        self.prefix = "risk-proof-" + uuid.uuid4().hex
        self.owned = {}
        self.images = {name: identity(image) for name, image in (
            ("postgres", POSTGRES), ("valkey", VALKEY), ("driver", "risk-proof:driver"),
            ("shared", "risk-proof:shared-dedup"), ("default", "risk-proof:default"))}

    def start(self, name, image, args, *, options=(), memory=384, cpu=".25", pids=18, tmp=64):
        network = "none" if name == "postgres" else "container:" + self.owned["postgres"]
        full = self.prefix+"-"+name
        result = docker("create", "--name", full, "--label", "kanz.risk-proof="+self.prefix,
            "--network", network, "--user", "65534:65534", "--read-only", "--cap-drop=ALL",
            "--security-opt=no-new-privileges", "--memory", f"{memory}m", "--memory-swap", f"{memory}m",
            "--cpus", cpu, "--pids-limit", str(pids), "--ulimit", "core=0:0", "--ulimit", "nofile=256:256",
            "--tmpfs", f"/tmp:rw,noexec,nosuid,nodev,uid=65534,gid=65534,mode=0700,size={tmp}m",
            "--log-opt", "max-size=256k", "--log-opt", "max-file=1", "-e", "GOMAXPROCS=1",
            *options, self.images[image], *args)
        container = result.stdout.strip()
        if not re.fullmatch("[0-9a-f]{64}", container):
            raise RuntimeError("invalid container identity")
        self.owned[name] = container
        config = json.loads(docker("inspect", "--format", "{{json .HostConfig}}",container).stdout)
        if (config["Privileged"] or not config["ReadonlyRootfs"] or config["CapDrop"] != ["ALL"]
                or "no-new-privileges" not in [opt.split(":")[0] for opt in config["SecurityOpt"]]
                or config["NetworkMode"] != network or config["PortBindings"]
                or config["Memory"] != memory*1024*1024 or config["MemorySwap"] != config["Memory"]
                or config["PidsLimit"] != pids):
            raise RuntimeError("container confinement mismatch: "+name)
        docker("start", container)
        return container

    def remove(self, name):
        container = self.owned[name]
        owner = docker("inspect","--format",'{{index .Config.Labels "kanz.risk-proof"}}',container).stdout.strip()
        if owner != self.prefix:
            raise RuntimeError("container ownership changed")
        docker("rm","-v","-f",container)
        del self.owned[name]

    def stop(self, name):
        container = self.owned[name]
        docker("stop","--time","20",container,timeout=24)
        code = docker("inspect","--format","{{.State.ExitCode}}",container).stdout.strip()
        if code != "0":
            raise RuntimeError(f"{name} did not stop gracefully (exit {code})")

    def once(self, name, image, args, options=(), expected=0):
        container = self.start(name,image,args,options=options)
        result = docker("wait",container,timeout=32).stdout.strip()
        output = docker("logs",container).stdout
        if result != str(expected):
            # Service logs can include configuration. Never export them.
            # Test failures are identified by reviewed Go test location only.
            sites = sorted(set(re.findall(r'(?:live|metrics|position)_test\.go:\d+',output)))
            raise RuntimeError(f"{name} exit {result}, expected {expected}; test locations {sites}")
        self.remove(name)
        return output

    def wait(self, name, ready):
        limit = time.monotonic()+25
        while time.monotonic()<limit:
            if ready(): return
            if docker("inspect","--format","{{.State.Running}}",self.owned[name]).stdout.strip() != "true":
                raise RuntimeError(name+" exited before readiness")
            time.sleep(.2)
        raise RuntimeError(name+" readiness timed out")

    def postgres(self):
        # UID 65534 owns the tmpfs. Only the synthetic non-bypass role can
        # connect over TCP. The admin socket lives solely in this container.
        init = """
set -eu
initdb -D /tmp/pg -U postgres --auth-local=trust --auth-host=reject >/dev/null
sed -i '1i host risk_proof risk_proof 127.0.0.1/32 trust' /tmp/pg/pg_hba.conf
exec postgres -D /tmp/pg -k /tmp -h 127.0.0.1
"""
        pg = self.start("postgres","postgres",["-c",init],options=["--entrypoint","/bin/sh",
             "--tmpfs","/var/lib/postgresql/data:rw,noexec,nosuid,nodev,size=1m"],memory=512,pids=28,tmp=192)
        self.wait("postgres", lambda: docker("exec",pg,"pg_isready","-h","/tmp","-U","postgres",check=False).returncode == 0)
        def sql(database, source):
            docker("exec","-i",pg,"psql","-X","-v","ON_ERROR_STOP=1","-U","postgres","-h","/tmp","-d",database,data=source)
        sql("postgres","CREATE ROLE risk_proof LOGIN NOSUPERUSER NOBYPASSRLS;\nCREATE DATABASE risk_proof;\n")
        migrations = sorted((ROOT/"kanz/services/risk_engine/migrations").glob("*.sql"))
        if [p.name[:4] for p in migrations] != ["0001","0002","0003","0004","0005"]:
            raise RuntimeError("risk migration inventory changed")
        for path in migrations:
            sql("risk_proof",path.read_text())
        sql("risk_proof","GRANT USAGE ON SCHEMA public TO risk_proof; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO risk_proof;")

    def broker(self):
        def mount(path, dest):
            return f"type=bind,src={ROOT/path},dst=/inputs/{dest},readonly"
        broker = self.start("broker","driver",["-test.run=^TestFixtureBroker$","-test.v","-test.timeout=6m"],
            options=["-e","KANZ_RISK_DEDUP_PROOF_PHASE=broker","--mount",mount("kanz/test/backing/nats-dev.conf","nats-dev.conf"),
                     "--mount",mount("kanz/infra/nats/bootstrap-job.yaml","streams.yaml")],memory=512,pids=20,tmp=192)
        self.wait("broker",lambda: "LOCAL_NATS_READY_2.15.3" in docker("logs",broker).stdout)

    def valkey(self):
        if "valkey" in self.owned:
            docker("start",self.owned["valkey"])
        else:
            self.start("valkey","valkey",["--bind","127.0.0.1","--save","","--appendonly","no","--dir","/tmp"],
                options=["--entrypoint","valkey-server","--tmpfs","/data:rw,noexec,nosuid,nodev,size=1m"],memory=96,cpu=".15",pids=12)
        self.wait("valkey",lambda: docker("exec",self.owned["valkey"],"valkey-cli","ping",check=False).stdout.strip()=="PONG")

    def risk(self, member):
        if member in self.owned:
            docker("start",self.owned[member])
            return
        port = "8081" if member=="proof-a" else "8082"
        cfg = {"LISTEN":"127.0.0.1:"+port,"GRPC_LISTEN":"127.0.0.1:"+("9081" if member=="proof-a" else "9082"),
            "NATS_URL":"nats://127.0.0.1:4222","SOURCE":member,"TENANT":"dedup-tenant",
            "DATABASE_URL":"postgres://risk_proof@127.0.0.1:5432/risk_proof?sslmode=disable","SHARD_MEMBERS":"proof-a,proof-b",
            "SHARD_SELF":member,"REDIS_URL":"redis://127.0.0.1:6379/0","SNAPSHOT_INTERVAL":"1h"}
        env = [arg for key,value in cfg.items() for arg in ("-e","RISK_ENGINE_"+key+"="+value)]
        self.start(member,"shared",[],options=env,memory=640,cpu=".5",pids=22)

    def phase(self,name,prior=None):
        env = ["-e","KANZ_RISK_DEDUP_PROOF_PHASE="+name, "-e", "KANZ_RISK_DEDUP_SOURCE="+os.environ["GITHUB_SHA"]]
        if prior: env.extend(["-e","KANZ_RISK_DEDUP_PRIOR_SEQUENCE="+prior])
        output = self.once("phase","driver",["-test.run=^TestLiveRiskDedupPhase$","-test.v","-test.timeout=25s"],options=env)
        if "--- PASS: TestLiveRiskDedupPhase " not in output or "SKIP" in output:
            raise RuntimeError("live phase did not run")
        print("PASS",name,flush=True)
        return output

    def run(self):
        self.postgres()
        controls = self.once("controls","driver",["-test.v","-test.run=^TestProof","-test.timeout=15s"])
        if len(re.findall(r"--- PASS: TestProof\S+ \(", controls)) != 16 or "SKIP" in controls:
            raise RuntimeError("driver control inventory changed")
        self.broker()
        self.valkey()
        result = self.once("default-refusal","default",[],options=["-e","RISK_ENGINE_REDIS_URL=redis://127.0.0.1:6379/0"],expected=2)
        if "requires the shared-dedup image" not in result:
            raise RuntimeError("default image failed for the wrong reason")
        self.risk("proof-a")
        self.phase("nonowner")
        self.risk("proof-b")
        self.phase("owner")
        self.phase("duplicates")
        self.stop("proof-b")
        result = self.phase("checkpoint")
        prior = re.findall(r"CHECKPOINT_EXPOSURE_SEQ=([1-9][0-9]*)",result)
        if len(prior)!=1: raise RuntimeError("missing checkpoint sequence")
        self.risk("proof-b")
        self.phase("restart",prior[0])
        self.stop("valkey")
        self.phase("outage")
        self.valkey()
        self.phase("recovery")
        self.phase("recovered-duplicates")
        self.stop("proof-b")
        self.phase("final-checkpoint")


def main():
    if os.environ.get("GITHUB_ACTIONS")!="true" or os.environ.get("GITHUB_EVENT_NAME")!="workflow_dispatch":
        raise RuntimeError("requires the manually dispatched disposable runner")
    sha = os.environ.get("GITHUB_SHA","")
    if not re.fullmatch("[0-9a-f]{40}",sha) or sha!=os.environ.get("EXPECTED_SHA") or command(["git","rev-parse","HEAD"]).stdout.strip()!=sha:
        raise RuntimeError("checkout differs from requested source")
    if sys.argv[1:]==["build"]:
        build(sha)
    elif sys.argv[1:]==["run"]:
        fixture=Fixture()
        try:
            fixture.run()
        finally:
            for name in reversed(list(fixture.owned)):
                fixture.remove(name)
    else:
        raise RuntimeError("expected build or run")


if __name__=="__main__":
    main()
