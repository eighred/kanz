"""Two complete serial internal/service passes on one unchanged canonical NATS.

This extends go-race shard 0's existing disposable fixture setup; it neither
starts a consumer qualification lane nor supplies a production runtime input.

The privileged workflow and both Python parent programs are a review prerequisite
at the exact checked head: kanz-ci.yml, position_reclaim_proof.py and
position_reclaim_sandbox.py. Child isolation does not contain modifications to
that workflow or either parent program.
"""
import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import socket
import subprocess
import sys
import time
import types
import uuid

# Bind unchanged host-side evidence consumers before importing them. Pins are
# their tracked LF bytes at canonical main 72370e66 (not a mutable Python path).
EVIDENCE_PINS={
    "go_test_evidence.py":"7cc929b153cf8446def7ef80d723808e1f077e64d454512e12886109e821b5ad",
    "go_shard_evidence.py":"d6539503f5dc992d4531cc85aa0d65f0f2353f2b6ba76468a07237e6448206ea",
}
for _name,_expected in EVIDENCE_PINS.items():
    _path=Path(__file__).resolve().with_name(_name)
    _fd=os.open(_path,os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        _before=os.fstat(_fd)
        if not stat.S_ISREG(_before.st_mode) or _before.st_nlink!=1 or _before.st_size>256*1024: raise ValueError("unsafe parent evidence import")
        _raw=os.read(_fd,256*1024+1)
        _after=os.fstat(_fd)
        if any(getattr(_before,k)!=getattr(_after,k) for k in ("st_dev","st_ino","st_mode","st_nlink","st_size","st_mtime_ns","st_ctime_ns")) or len(_raw)!=_before.st_size or hashlib.sha256(_raw).hexdigest()!=_expected: raise ValueError("parent evidence consumer differs from canonical main")
        _module=types.ModuleType(_path.stem)
        _module.__file__=str(_path)
        sys.modules[_path.stem]=_module
        exec(compile(_raw,str(_path),"exec"),_module.__dict__)
    finally: os.close(_fd)

from go_shard_evidence import identity, inventory, validate_events, digest, MAX_EVENTS
from go_test_evidence import check, events
from position_reclaim_sandbox import Sandbox, directory, read_regular, new_file, host, SAFE

PREFIX = "github.com/eighred/kanz/"
STORE = PREFIX+"services/oms/tools/store-conformance"
CONTROL = PREFIX+"internal/bustest"
HELPERS={CONTROL,CONTROL+"/positionfixture"}
CONTROLLER = "./test/backing/position_reclaim"
CAUSAL_CASES={
    (PREFIX+"services/webhook-ingest/internal/ingest",name) for name in (
        "TestIntegration_LoopOverNATS","TestIntegration_ALargeQuantityKeepsItsMagnitudeOverTheBusAndInPostgres",
        "TestIntegration_AnOutOfDomainDecimalIsRefusedAndTheOMSKeepsConsuming")
} | {
    (PREFIX+"services/venue-binance/internal/binance","TestOverfillReachesNoBroker"),
    (PREFIX+"services/venue-binance/internal/binance","TestExecutedPriceRefusalReachesNoBroker"),
    (PREFIX+"services/venue-okx/internal/okx","TestExecutedPriceAndSignedFeesOnRealBroker"),
    (PREFIX+"services/oms/internal/order","TestRecoveryHandlerRestartsAtFrozenEvidenceOverRealBroker"),
}
ALLOW = (
    "TEST_POSTGRES_URL", "TEST_NATS_URL", "TEST_TV_NATS_URL", "TEST_KAFKA_BROKERS",
    "TEST_OMS_ON_BUS", "TEST_INFERENCE_ON_BUS", "TEST_OMS_POSTGRES_URL",
    "TEST_SESSION_BROWSER_SCRIPT", "TEST_MFA_BROWSER_SCRIPT", "TEST_MFA_PLAYWRIGHT",
    "TEST_MFA_NODE", "TEST_MFA_BFF", "TEST_MFA_STATIC_DIR", "TEST_NATS_MTLS_URL",
    "TEST_NATS_MTLS_CERT_DIR",
)
EXPECTED_ENDPOINTS = {
    "TEST_POSTGRES_URL":"postgres://kanzapp:kanz@localhost:5432/kanzapp?sslmode=disable",
    "TEST_NATS_URL":"nats://localhost:4222", "TEST_TV_NATS_URL":"nats://127.0.0.1:4224",
    "TEST_KAFKA_BROKERS":"localhost:9092", "TEST_OMS_ON_BUS":"1", "TEST_INFERENCE_ON_BUS":"1",
    "TEST_OMS_POSTGRES_URL":"postgres://kanzapp:kanz@localhost:5432/kanz_oms_ci?sslmode=disable",
    "TEST_NATS_MTLS_URL":"nats://localhost:4223", "TEST_NATS_MTLS_CERT_DIR":"/tmp/kanz-mtls",
}


def docker_identity(name="kanz-ci-nats"):
    raw = json.loads(host("docker","inspect",name))
    if len(raw)!=1: raise ValueError("ambiguous owned broker")
    row = raw[0]
    if not row["State"]["Running"] or row["State"]["Restarting"]:
        raise ValueError("disposable broker is not running")
    return {"container":row["Id"],"image":row["Image"],"started":row["State"]["StartedAt"],
            "restart_count":row["RestartCount"],"name":row["Name"]}


def stable_broker(before, after):
    if before!=after: raise ValueError("canonical broker container/image/start identity changed")


def validate_negative_mounts(row, data=None):
    """Refuse image-declared volumes and any unassigned writable mount."""
    seen=set()
    for mount in row["Mounts"]:
        destination=mount["Destination"]
        if destination in seen: raise ValueError("duplicate negative fixture mount")
        seen.add(destination)
        if mount["Type"]=="bind":
            if data is None or destination!="/data" or mount["Source"]!=str(data) or mount["RW"] is not True: raise ValueError("unassigned negative fixture bind")
        elif mount["Type"]=="tmpfs":
            if destination=="/dev/shm":
                if mount["RW"]: raise ValueError("negative fixture shared memory is writable")
            elif destination!="/tmp" or data is not None or mount["RW"] is not True: raise ValueError("unassigned negative fixture tmpfs")
        else: raise ValueError("anonymous or extra negative fixture volume")
    if data is not None and "/data" not in seen: raise ValueError("owned negative data bind absent")


def require_native_cases(lines):
    native = [e for e in events(lines) if e.get("Package")==STORE]
    roots = {e.get("Test","") for e in native if e["Action"]=="run" and "/" not in e.get("Test","")}
    required = {"TestRustPositionStorePostgresNATS","TestRustRelayPostgresNATS"}
    if not required <= roots or not any("Projector" in name for name in roots):
        raise ValueError("actual native position/relay/projector cases were not inventoried")
    skipped = [e.get("Test","") for e in native if e["Action"]=="skip"]
    if skipped: raise ValueError("actual stamped Rust store suite skipped a case")
    return sorted(roots)


def pass_result(raw, packages):
    text = raw.decode("utf-8")
    validate_events(text,packages)
    problems,skipped,count = check(text.splitlines(),packages)
    if problems: raise ValueError("; ".join(problems))
    native = require_native_cases(text.splitlines())
    causal={(e.get("Package"),e.get("Test")) for e in events(text.splitlines()) if "causal live-OMS POSITION receipts:" in e.get("Output","")}
    passed={(e.get("Package"),e.get("Test")) for e in events(text.splitlines()) if e["Action"]=="pass"}
    if not CAUSAL_CASES<=causal or not CAUSAL_CASES<=passed: raise ValueError("owned indirect publisher causal receipt or passed result missing")
    skipped_cases = sorted({(e.get("Package",""),e.get("Test","")) for e in events(text.splitlines()) if e["Action"]=="skip"})
    return {"package_count":count,"skip_count":skipped,"skipped_cases":skipped_cases,"native_store_cases":native,"causal_receipt_cases":sorted(causal)}


def parse_objects(raw):
    text,index,decoder = raw.decode(),0,json.JSONDecoder()
    while index<len(text):
        while index<len(text) and text[index].isspace(): index+=1
        if index==len(text): return
        value,index = decoder.raw_decode(text,index)
        yield value


class Proof:
    def __init__(self, args):
        self.args=args
        self.repo=Path.cwd().resolve().parent
        # sudo changes ownership context, never broaden Git trust globally.
        os.environ.update(GIT_CONFIG_COUNT="1",GIT_CONFIG_KEY_0="safe.directory",GIT_CONFIG_VALUE_0=str(self.repo))
        self.source=identity()
        self.root=Path(os.environ["RUNNER_TEMP"])/("position-reclaim-"+uuid.uuid4().hex)
        self.root.mkdir(mode=0o700)
        self.started=time.monotonic()
        self.box=None
        self.before=None
        self.negative_id=None
        self.network_id=None
        self.negative_name="position-negative-"+uuid.uuid4().hex[:12]
        self.network_name=self.negative_name+"-net"
        self.sequence=0
        self.seed=None
        self.cleaned=False
        self.data_mounted=False
        self.bootstrap_id=None
        self.negative_receipts=[]

    def invoke(self,args,**kwargs):
        if time.monotonic()-self.started>7800: raise ValueError("proof exceeded its 130-minute parent budget")
        return self.box.execute(args,**kwargs)

    def successful(self,args,**kwargs):
        code,raw=self.invoke(args,**kwargs)
        if code:
            self.box.retain("failed-command-%03d.log"%self.sequence,raw)
            self.sequence+=1
            raise ValueError("bounded source command failed (see protected command evidence)")
        return raw

    def controller(self,phase,name,*,negative=False,expect=None):
        args=["/fixture/position-reclaim","--phase",phase,"--output","/scratch/receipt.json"]
        mounts=[]
        if phase!="seed":
            args += ["--seed","/receipt/seed.json"]
            mounts=[(self.box.out/("negative-seed.json" if negative else "seed.json"),"/receipt/seed.json")]
        extra={"TEST_NATS_URL":"nats://localhost:4225"} if negative else {}
        code,raw=self.invoke(args,seconds=35,mounts=mounts,extra=extra,output_limit=65536)
        if expect:
            if code==0 or expect not in raw.decode(errors="replace"): raise ValueError("controller negative control did not discriminate")
            self.box.retain(name+".log",raw)
            return None
        if code: raise ValueError("sentinel controller refused "+phase+" (no endpoint diagnostics)")
        receipt=json.loads(self.box.promote("receipt.json",name+".json",16384))
        if phase=="seed": (self.box.out/(name+".json")).chmod(0o444)
        os.unlink("receipt.json",dir_fd=self.box.work_fd)
        return receipt

    def prepare(self):
        if os.environ.get("GITHUB_ACTIONS")!="true" or os.environ.get("RUNNER_OS")!="Linux":
            raise ValueError("only the existing disposable Linux Actions runner is authorized")
        if any(os.environ.get(k)!=v for k,v in EXPECTED_ENDPOINTS.items()):
            raise ValueError("missing or unbound existing disposable backing service")
        if any(not os.environ.get(key) for key in ALLOW): raise ValueError("required fixture variable missing")
        host("git","-c","safe.directory="+str(self.repo),"-C",str(self.repo),"diff","--exit-code","--",".",":(exclude)kanz/go.sum")
        self.before=docker_identity()
        # These are trusted tool inspections/downloaded inputs, not source builds.
        go=Path(self.args.go).resolve()
        go_env=json.loads(subprocess.check_output([str(go),"env","-json","GOROOT","GOMODCACHE","GOVERSION","GOOS","GOARCH","CGO_ENABLED"],env=dict(SAFE,GOTOOLCHAIN="local",GOMODCACHE=self.args.gomodcache)))
        if go_env["GOVERSION"]!="go1.26.10" or go_env["GOOS"]!="linux" or go_env["GOARCH"]!="amd64" or go_env["CGO_ENABLED"]!="1": raise ValueError("actual pinned Go Linux/cgo toolchain differs")
        rust=Path(self.args.rustc).resolve().parent.parent
        rust_version=host(str(rust/"bin/rustc"),"--version").decode().strip()
        if not rust_version.startswith("rustc 1.90.0 "): raise ValueError("actual reviewed Rust compiler differs")
        node=Path(self.args.node).resolve().parent.parent
        pg=host("psql",EXPECTED_ENDPOINTS["TEST_POSTGRES_URL"],"-tAX","-c","SELECT current_user,rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").strip()
        if pg!=b"kanzapp|f|f": raise ValueError("actual database principal bypasses tenant RLS")
        tools=[(go_env["GOROOT"],"/tool/go"),(rust,"/tool/rust"),(node,"/tool/node"),
               (go_env["GOMODCACHE"],"/deps/gomod"),(Path(self.args.cargo_home)/"registry","/scratch/cargo/registry"),
               (self.repo/"kanz-schemas/gen/go","/input/kanz-schemas/gen/go"),
               (self.repo/"kanz-web/node_modules","/input/kanz-web/node_modules"),
               (self.repo/"kanz-web/dist","/input/kanz-web/dist"),
               (Path(self.args.browser),"/deps/browser"),(os.environ["TEST_MFA_BFF"],"/fixture/mfa-web-bff"),
               ("/tmp/kanz-mtls","/fixture/mtls")]
        env={"PATH":"/tool/go/bin:/tool/rust/bin:/tool/node/bin:/usr/bin:/bin","LANG":"C.UTF-8","HOME":"/scratch/home","TMPDIR":"/scratch/tmp","GOTMPDIR":"/scratch/tmp","GOCACHE":"/scratch/cache","GOMODCACHE":"/deps/gomod","GOPATH":"/scratch/go","GOTOOLCHAIN":"local","GOPROXY":"off","GOFLAGS":"-mod=readonly -p=1","GOMAXPROCS":"2","CARGO_HOME":"/scratch/cargo","CARGO_TARGET_DIR":"/scratch/rust-target","CARGO_NET_OFFLINE":"true","KANZ_BUILD_VERSION":self.source["sha"],"PLAYWRIGHT_BROWSERS_PATH":"/deps/browser","CI":"true","CGO_ENABLED":"1","GITHUB_ACTIONS":"true","GITHUB_SHA":self.source["sha"],"GITHUB_RUN_ID":self.source["run"],"GITHUB_RUN_ATTEMPT":self.source["attempt"],"GIT_CONFIG_COUNT":"1","GIT_CONFIG_KEY_0":"safe.directory","GIT_CONFIG_VALUE_0":"/input"}
        env.update(EXPECTED_ENDPOINTS)
        # The unchanged native fixture deliberately accepts numeric loopback
        # only. Both names reach the same fixed namespace-local broker relay.
        env["TEST_NATS_URL"]="nats://127.0.0.1:4222"
        env.update(TEST_SESSION_BROWSER_SCRIPT="/input/kanz-web/e2e/sessions.cjs",TEST_MFA_BROWSER_SCRIPT="/input/kanz-web/e2e/mfa.cjs",TEST_MFA_PLAYWRIGHT="/input/kanz-web/node_modules/playwright",TEST_MFA_NODE="/tool/node/bin/node",TEST_MFA_BFF="/fixture/mfa-web-bff",TEST_MFA_STATIC_DIR="/input/kanz-web/dist",TEST_NATS_MTLS_CERT_DIR="/fixture/mtls")
        self.box=Sandbox(self.repo,self.root,tools,env)
        self.box.start()
        self.box.probe()
        provenance={"source":self.source,"go":go_env,"go_sha256":digest(go.read_bytes()),"rust":rust_version,"rustc_sha256":digest((rust/"bin/rustc").read_bytes()),"cc_sha256":digest(Path("/usr/bin/cc").resolve().read_bytes()),"libc_sha256":digest(Path("/lib/x86_64-linux-gnu/libc.so.6").read_bytes()),"broker":self.before,"database_role":{"name":"kanzapp","superuser":False,"bypassrls":False},"sandbox":{"cpu_percent":200,"memory_bytes":6*1024**3,"tasks":512,"scratch_bytes":5*1024**3,"child_seconds":540,"parent_seconds":7800,"relay_ports":[4222,4223,4224,4225,5432,6379,9092]},"environment_names":sorted(env)}
        provenance["evidence_consumer_pins"]=EVIDENCE_PINS
        provenance["nats_endpoint_mapping"]={"host":"localhost:4222","sandbox":"127.0.0.1:4222","same_fixed_relay":4222}
        source_fd=directory(self.box.inputs/"source")
        try:
            tested_sum=read_regular(source_fd,"kanz/go.sum",4*1024*1024)
            go_mod=read_regular(source_fd,"kanz/go.mod",1024*1024)
            cargo_lock=read_regular(source_fd,"kanz-rs/Cargo.lock",4*1024*1024)
        finally: os.close(source_fd)
        tracked_sum=host("git","-c","safe.directory="+str(self.repo),"-C",str(self.repo),"show",self.source["sha"]+":kanz/go.sum")
        provenance["go_sum"]={"tested_sha256":digest(tested_sum),"tracked_sha256":digest(tracked_sum),"changed_by_provisioning":tested_sum!=tracked_sum}
        provenance["go_mod_sha256"]=digest(go_mod)
        provenance["cargo_lock_sha256"]=digest(cargo_lock)
        sdk_fd=directory(self.repo/"kanz-schemas/gen/go")
        sdk={};total=0
        try:
            for base,dirs,files in os.walk(self.repo/"kanz-schemas/gen/go",followlinks=False):
                if any((Path(base)/d).is_symlink() for d in dirs): raise ValueError("linked generated SDK directory")
                for name in sorted(files):
                    relative=str((Path(base)/name).relative_to(self.repo/"kanz-schemas/gen/go"))
                    raw=read_regular(sdk_fd,relative,32*1024*1024)
                    total+=len(raw)
                    if len(sdk)>=10000 or total>128*1024*1024: raise ValueError("generated SDK inventory exceeds bound")
                    sdk[relative]=digest(raw)
        finally: os.close(sdk_fd)
        self.box.retain("generated-sdk-inputs.json",json.dumps(sdk,sort_keys=True).encode())
        provenance["generated_sdk_manifest_sha256"]=digest(json.dumps(sdk,sort_keys=True).encode())
        packages=host("dpkg-query","-W","-f=${Package}\t${Version}\t${Architecture}\n")
        self.box.retain("sysroot-packages.txt",packages)
        provenance["sysroot_packages_sha256"]=digest(packages)
        self.box.retain("provenance.json",json.dumps(provenance,sort_keys=True).encode())
        self.successful(["cargo","build","--frozen","-p","kanz-oms-store","--example","fixture"],cwd="/input/kanz-rs",seconds=540)
        binary=self.box.promote("rust-target/debug/examples/fixture","oms-store",128*1024*1024)
        (self.box.out/"oms-store").chmod(0o555)
        # Build products are not dependency inputs for subsequent commands.
        self.successful(["rm","-rf","/scratch/rust-target"],seconds=30)
        self.successful(["go","build","-o","/scratch/controller",CONTROLLER])
        controller=self.box.promote("controller","position-reclaim",32*1024*1024)
        (self.box.out/"position-reclaim").chmod(0o555)
        self.box.tools += [(self.box.out/"oms-store","/fixture/oms-store"),(self.box.out/"position-reclaim","/fixture/position-reclaim")]
        self.box.environment.update(KANZ_OMS_STORE_BIN="/fixture/oms-store",KANZ_OMS_STORE_VERSION=self.source["sha"])
        self.box.retain("fixture-binaries.json",json.dumps({"source":self.source,"oms_store_sha256":digest(binary),"controller_sha256":digest(controller)}).encode())

    def graph(self):
        modules=list(parse_objects(self.successful(["go","list","-m","-json","all"])))
        if not modules or any(row.get("Error") for row in modules): raise ValueError("incomplete module/replacement graph")
        module_inputs=[]
        for row in modules:
            compact={k:row[k] for k in ("Path","Version","Sum","GoModSum","Main") if k in row}
            if "Replace" in row: compact["Replace"]={k:row["Replace"][k] for k in ("Path","Version","Sum","GoModSum") if k in row["Replace"]}
            module_inputs.append(compact)
        self.box.retain("module-checksum-inputs.json",json.dumps({"source":self.source,"modules":module_inputs},sort_keys=True).encode())
        raw=self.successful(["go","list","-deps","-test","-export","-json","./..."])
        rows=list(parse_objects(raw))
        if not rows or any(r.get("Incomplete") or r.get("Error") or r.get("DepsErrors") for r in rows): raise ValueError("incomplete compiled import graph")
        runtime=[r for r in rows if r.get("Module",{}).get("Path")==PREFIX[:-1] and " [" not in r["ImportPath"] and not r["ImportPath"].endswith(".test")]
        importers=[{"consumer":r["ImportPath"],"helper":helper} for r in runtime if r["ImportPath"] not in HELPERS for helper in sorted(HELPERS) if helper in r.get("Imports",[])]
        if importers: raise ValueError("production runtime acquired test cleanup dependency")
        internal_edges=[{"consumer":r["ImportPath"],"helper":helper} for r in runtime if r["ImportPath"] in HELPERS for helper in sorted(HELPERS) if helper in r.get("Imports",[])]
        compact=[{k:r.get(k,[]) for k in ("ImportPath","GoFiles","Imports","TestGoFiles","TestImports","XTestGoFiles","XTestImports")} for r in runtime]
        self.box.retain("runtime-import-map.json",json.dumps({"source":self.source,"runtime_importers":importers,"declared_test_support_dependencies":internal_edges,"packages":compact},sort_keys=True).encode())

    def guard(self):
        raw=self.successful(["python3","-B","-m","unittest","discover","-s",".github/scripts","-p","test_position_reclaim.py"],cwd="/input",seconds=60)
        self.box.retain("parent-refusal-tests.log",raw)
        raw=self.successful(["go","test","-race","-p","1","./test/arch",CONTROLLER,"-run","^Test(Position|Receipt)","-count=1","-timeout=8m","-json"])
        validate_events(raw.decode(),[PREFIX+"test/arch",PREFIX+"test/backing/position_reclaim"])
        self.box.retain("guard-events.jsonl",raw)

    def run_pass(self,number,packages):
        combined=bytearray()
        for index,package in enumerate(packages):
            code,raw=self.invoke(["go","test","-race","-p","1","-count=1","-timeout=8m","-json",package])
            combined.extend(raw)
            if len(combined)>MAX_EVENTS: raise ValueError("complete pass event bound exceeded")
            if code:
                self.box.retain("pass-%d-events.jsonl"%number,bytes(combined))
                raise ValueError("complete pass failed in "+package)
            validate_events(raw.decode(),[package])
            if (index+1)%25==0: print("POSITION pass %d: %d/%d packages completed"%(number,index+1,len(packages)),flush=True)
        raw=bytes(combined)
        summary=pass_result(raw,packages)
        snapshot=self.controller("snapshot","after-pass-%d"%number)
        stable_broker(self.before,docker_identity())
        summary.update(source=self.source,pass_number=number,packages_sha256=digest(("\n".join(packages)+"\n").encode()),events_sha256=digest(raw),broker=docker_identity(),snapshot=snapshot)
        self.box.retain("pass-%d-events.jsonl"%number,raw)
        self.box.retain("pass-%d-manifest.json"%number,json.dumps(summary,sort_keys=True).encode())
        print("POSITION pass %d PASS: %d packages, canonical sentinel unchanged, retained subjects=%d"%(number,len(packages),snapshot["subject_count"]),flush=True)

    def negative_start(self):
        self.network_id=host("docker","network","create","--internal","--label","kanz.position-reclaim="+self.source["run"],self.network_name).decode().strip()
        self.data=self.root/"negative-data"
        self.data.mkdir(mode=0o700)
        host("mount","-t","tmpfs","-o","size=128M,nodev,nosuid,noexec,mode=0700,uid=65534,gid=65534","position-negative",str(self.data))
        self.data_mounted=True
        network=json.loads(host("docker","network","inspect",self.network_id))[0]
        if not network["Internal"] or network["Labels"].get("kanz.position-reclaim")!=self.source["run"]: raise ValueError("negative network is not owned/internal")
        self.negative_create()

    def negative_create(self):
        self.negative_id=host("docker","create","--pull=never","--name",self.negative_name,"--network",self.network_name,"--label","kanz.position-reclaim="+self.source["run"],"--memory=256m","--cpus=1","--pids-limit=128","--read-only","--cap-drop=ALL","--security-opt=no-new-privileges","--ulimit","core=0:0","--ulimit","nofile=256:256","--ulimit","fsize=134217728:134217728","--ulimit","nproc=128:128","--user=65534:65534","--entrypoint=/usr/bin/env","--tmpfs","/dev/shm:ro,nosuid,nodev,noexec,size=64k","-v",str(self.data)+":/data:rw","-p","127.0.0.1:4225:4222",self.before["image"],"-i","PATH=/usr/local/bin:/usr/bin:/bin","HOME=/data","/nats-server","-js","-sd","/data").decode().strip()
        actual=json.loads(host("docker","inspect",self.negative_id))[0]
        hc=actual["HostConfig"]
        limits={r["Name"]:(r["Soft"],r["Hard"]) for r in hc["Ulimits"]}
        validate_negative_mounts(actual,self.data)
        if hc["Tmpfs"]!={"/dev/shm":"ro,nosuid,nodev,noexec,size=64k"}: raise ValueError("negative broker shared-memory mount differs")
        if actual["State"]["Running"] or actual["Config"]["Entrypoint"]!=["/usr/bin/env"] or actual["Config"]["Cmd"]!=["-i","PATH=/usr/local/bin:/usr/bin:/bin","HOME=/data","/nats-server","-js","-sd","/data"] or actual["Config"]["User"]!="65534:65534" or not hc["ReadonlyRootfs"] or hc["Memory"]!=256*1024**2 or hc["NanoCpus"]!=1000000000 or hc["PidsLimit"]!=128 or hc["CapDrop"]!=["ALL"] or hc["SecurityOpt"]!=["no-new-privileges"] or set(actual["NetworkSettings"]["Networks"])!={self.network_name} or actual["Image"]!=self.before["image"] or hc["Binds"]!=[str(self.data)+":/data:rw"] or hc["PortBindings"]!={"4222/tcp":[{"HostIp":"127.0.0.1","HostPort":"4225"}]} or limits!={"core":(0,0),"nofile":(256,256),"fsize":(134217728,134217728),"nproc":(128,128)}:
            raise ValueError("actual negative broker isolation differs")
        self.negative_receipts.append({"phase":"created-before-start","container":self.negative_id,"image":actual["Image"],"network":self.network_id,"user":actual["Config"]["User"],"memory":hc["Memory"],"nano_cpus":hc["NanoCpus"],"pids":hc["PidsLimit"],"ulimits":limits,"entrypoint":actual["Config"]["Entrypoint"],"command":actual["Config"]["Cmd"]})
        host("docker","start",self.negative_id)
        for _ in range(30):
            try:
                with socket.create_connection(("127.0.0.1",4225),timeout=.25): pass
                break
            except OSError: time.sleep(.25)
        else: raise ValueError("owned negative broker did not become ready")
        box_image=host("docker","image","inspect","--format","{{.Id}}","natsio/nats-box:0.14.5").decode().strip()
        source_fd=directory(self.box.inputs/"source")
        try: config=read_regular(source_fd,"kanz/infra/nats/bootstrap-job.yaml",256*1024).decode()
        finally: os.close(source_fd)
        if config.count("  bootstrap.sh: |\n")!=1: raise ValueError("canonical bootstrap block differs")
        body=config.split("  bootstrap.sh: |\n",1)[1].split("\n---",1)[0]
        if any(line and not line.startswith("    ") for line in body.splitlines()): raise ValueError("canonical bootstrap indentation differs")
        script=("\n".join(line[4:] if line else "" for line in body.splitlines())+"\n").encode()
        bootstrap=host("docker","create","-i","--pull=never","--name",self.negative_name+"-bootstrap","--label","kanz.position-reclaim="+self.source["run"],"--network",self.network_name,"--memory=256m","--cpus=1","--pids-limit=128","--read-only","--cap-drop=ALL","--security-opt=no-new-privileges","--ulimit","core=0:0","--ulimit","nofile=256:256","--ulimit","fsize=33554432:33554432","--ulimit","nproc=128:128","--user=65534:65534","--entrypoint=/usr/bin/env","--tmpfs","/tmp:rw,noexec,nosuid,size=32m,uid=65534,gid=65534","--tmpfs","/dev/shm:ro,nosuid,nodev,noexec,size=64k",box_image,"-i","PATH=/usr/local/bin:/usr/bin:/bin","HOME=/tmp","NATS_URL=nats://"+self.negative_name+":4222","NATS_REPLICAS=1","sh","-s").decode().strip()
        self.bootstrap_id=bootstrap
        try:
            created=json.loads(host("docker","inspect",bootstrap))[0]
            bh=created["HostConfig"]
            bl={r["Name"]:(r["Soft"],r["Hard"]) for r in bh["Ulimits"]}
            validate_negative_mounts(created)
            if created["State"]["Running"] or created["Config"]["User"]!="65534:65534" or created["Config"]["Entrypoint"]!=["/usr/bin/env"] or created["Config"]["Cmd"]!=["-i","PATH=/usr/local/bin:/usr/bin:/bin","HOME=/tmp","NATS_URL=nats://"+self.negative_name+":4222","NATS_REPLICAS=1","sh","-s"] or not bh["ReadonlyRootfs"] or bh["Memory"]!=256*1024**2 or bh["NanoCpus"]!=1000000000 or bh["PidsLimit"]!=128 or bh["CapDrop"]!=["ALL"] or bh["SecurityOpt"]!=["no-new-privileges"] or bh["Tmpfs"]!={"/tmp":"rw,noexec,nosuid,size=32m,uid=65534,gid=65534","/dev/shm":"ro,nosuid,nodev,noexec,size=64k"} or set(created["NetworkSettings"]["Networks"])!={self.network_name} or bl!={"core":(0,0),"nofile":(256,256),"fsize":(33554432,33554432),"nproc":(128,128)}: raise ValueError("actual bootstrap isolation differs")
            self.negative_receipts.append({"phase":"bootstrap-created-before-start","container":bootstrap,"image":created["Image"],"user":created["Config"]["User"],"ulimits":bl,"bootstrap_sha256":digest(script)})
            host("docker","start","-a","-i",bootstrap,input=script,timeout=60)
        finally:
            row=json.loads(host("docker","inspect",bootstrap))[0]
            if row["Name"]!="/"+self.negative_name+"-bootstrap" or row["Config"]["Labels"].get("kanz.position-reclaim")!=self.source["run"]: raise ValueError("bootstrap container ownership differs")
            host("docker","rm","-f",bootstrap)
            if host("docker","ps","-a","--no-trunc","--filter","id="+bootstrap,"--format","{{.ID}}").strip(): raise ValueError("bootstrap container remains")
            self.negative_receipts.append({"phase":"removed","container":bootstrap,"remaining":[]})
            self.bootstrap_id=None

    def negative_remove(self):
        if self.negative_id:
            actual=json.loads(host("docker","inspect",self.negative_id))[0]
            if actual["Config"]["Labels"].get("kanz.position-reclaim")!=self.source["run"] or actual["Name"]!="/"+self.negative_name: raise ValueError("negative broker ownership differs")
            host("docker","rm","-f",self.negative_id)
            if host("docker","ps","-a","--no-trunc","--filter","id="+self.negative_id,"--format","{{.ID}}").strip(): raise ValueError("negative broker remains")
            self.negative_receipts.append({"phase":"removed","container":self.negative_id,"remaining":[]})
            self.negative_id=None

    def controls(self):
        self.negative_start()
        original=(self.repo/"kanz/internal/bustest/reclaim.go").read_text()
        for label,mutant,reason in (
            ("missing-cleanup",original.replace("t.Helper()","t.Helper(); return",1),"owned subject survived cleanup"),
            ("global-purge",original.replace("jetstream.WithPurgeSubject(subj)","jetstream.WithPurgeSubject(\">\")"),"unrelated canonical sentinel was removed or changed"),
        ):
            if mutant==original: raise ValueError("cleanup mutation did not change source")
            path=self.box.inputs/(label+".go")
            path.write_text(mutant)
            code,raw=self.invoke(["go","test","-race","-p","1",CONTROL,"-run","^TestReclaimPreservesAnUnrelatedCanonicalPosition$","-count=1","-timeout=1m","-json"],seconds=100,extra={"TEST_NATS_URL":"nats://localhost:4225"},mounts=[(path,"/input/kanz/internal/bustest/reclaim.go")])
            ev=list(events(raw.decode().splitlines()))
            if code==0 or not any(e["Action"]=="fail" and e.get("Test")=="TestReclaimPreservesAnUnrelatedCanonicalPosition" for e in ev) or reason not in raw.decode(): raise ValueError("cleanup negative control failed to discriminate: "+label)
            self.box.retain(label+"-events.jsonl",raw)
            self.box.retain(label+"-source.json",json.dumps({"mutated_sha256":digest(mutant.encode()),"expected_failure":reason}).encode())
            self.negative_remove()
            # Only this explicitly owned negative fixture is recreated; never
            # the primary broker used for either complete suite pass.
            host("umount",str(self.data))
            self.data_mounted=False
            host("mount","-t","tmpfs","-o","size=128M,nodev,nosuid,noexec,mode=0700,uid=65534,gid=65534","position-negative",str(self.data))
            self.data_mounted=True
            self.negative_create()
        seeded=self.controller("seed","negative-seed",negative=True)
        old=docker_identity(self.negative_id)
        self.negative_remove()
        self.negative_create()  # Same storage; stream creation must be retained.
        changed=docker_identity(self.negative_id)
        if old["container"]==changed["container"]: raise ValueError("recreation control was vacuous")
        self.controller("snapshot","recreated-broker-refusal",negative=True,expect="broker or stream was replaced/restarted")
        self.box.retain("recreated-broker.json",json.dumps({"before":old,"after":changed,"retained_stream_created":seeded["stream_created"]}).encode())
        stable_broker(self.before,docker_identity())

    def run(self):
        self.prepare()
        self.graph()
        self.guard()
        packages=inventory(self.successful(["go","list","-race","./internal/...","./services/...",CONTROLLER]).decode())
        self.box.retain("packages.txt",("\n".join(packages)+"\n").encode())
        self.seed=self.controller("seed","seed")
        self.run_pass(1,packages)
        self.run_pass(2,packages)
        self.controller("cleanup","sentinel-cleanup")
        self.cleaned=True
        self.controls()
        self.box.retain("passes-complete.json",json.dumps({"source":self.source,"complete_passes":2,"packages_per_pass":len(packages),"same_broker":docker_identity(),"negative_controls":["missing-cleanup","global-purge","recreation-with-retained-storage"]},sort_keys=True).encode())

    def close(self):
        failures=[]
        if self.box and self.seed and not self.cleaned:
            try: self.controller("cleanup","sentinel-failure-cleanup")
            except BaseException as error: failures.append(type(error).__name__)
        if self.bootstrap_id:
            try:
                row=json.loads(host("docker","inspect",self.bootstrap_id))[0]
                if row["Name"]!="/"+self.negative_name+"-bootstrap" or row["Config"]["Labels"].get("kanz.position-reclaim")!=self.source["run"]: raise ValueError("bootstrap ownership differs")
                host("docker","rm","-f",self.bootstrap_id)
                if host("docker","ps","-a","--no-trunc","--filter","id="+self.bootstrap_id,"--format","{{.ID}}").strip(): raise ValueError("bootstrap remains")
                self.bootstrap_id=None
            except BaseException as error: failures.append(type(error).__name__)
        try: self.negative_remove()
        except BaseException as error: failures.append(type(error).__name__)
        if self.network_id:
            try:
                row=json.loads(host("docker","network","inspect",self.network_id))[0]
                if row["Labels"].get("kanz.position-reclaim")!=self.source["run"] or row["Containers"]: raise ValueError("negative network ownership/emptiness differs")
                host("docker","network","rm",self.network_id)
                if host("docker","network","ls","--no-trunc","--filter","id="+self.network_id,"--format","{{.ID}}").strip(): raise ValueError("negative network remains")
                self.network_id=None
            except BaseException as error: failures.append(type(error).__name__)
        if self.data_mounted:
            try:
                host("umount",str(self.data))
                self.data_mounted=False
            except BaseException as error: failures.append(type(error).__name__)
        if self.box and self.box.out_fd is not None:
            try: self.box.retain("negative-fixture-lifecycle.json",json.dumps({"source":self.source,"receipts":self.negative_receipts,"remaining_container":self.negative_id,"remaining_bootstrap":self.bootstrap_id,"remaining_network":self.network_id,"data_mounted":self.data_mounted,"cleanup_errors":failures},sort_keys=True).encode())
            except BaseException as error: failures.append(type(error).__name__)
        if self.box:
            try: self.box.close()
            except BaseException as error: failures.append(type(error).__name__)
        if failures: raise ValueError("proof cleanup failed: "+",".join(failures))


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ("go","gomodcache","rustc","node","cargo-home","browser"): parser.add_argument("--"+name,required=True)
    proof=Proof(parser.parse_args())
    primary=None
    try: proof.run()
    except BaseException as error:
        primary=error
        raise
    finally:
        cleanup_error=None
        try: proof.close()
        except BaseException as cleanup:
            cleanup_error=cleanup
            print("POSITION cleanup also refused:",type(cleanup).__name__,file=sys.stderr)
        # Root-only intermediates are excluded from upload. Give the runner
        # read access only after all targets have terminated.
        if proof.box and proof.box.out.exists():
            proof.root.chmod(0o711)
            proof.box.out.chmod(0o755)
            for path in proof.box.out.iterdir():
                if path.name in ("oms-store","position-reclaim"): path.unlink()
                elif path.is_file() and not path.is_symlink(): path.chmod(0o644)
            if primary is None and cleanup_error is None:
                fd=directory(proof.box.out)
                try:
                    hashes={}
                    total=0
                    for path in sorted(proof.box.out.iterdir()):
                        raw=read_regular(fd,path.name,MAX_EVENTS)
                        total+=len(raw)
                        if total>768*1024*1024: raise ValueError("final artifact set exceeds bound")
                        hashes[path.name]=digest(raw)
                    new_file(fd,"complete.json",json.dumps({"source":proof.source,"artifacts_sha256":hashes,"cleanup":"observed-complete"},sort_keys=True).encode(),mode=0o644)
                finally: os.close(fd)
        print("POSITION protected evidence:",str(proof.root/"artifacts"),flush=True)
        if cleanup_error is not None and primary is None: raise cleanup_error


if __name__=="__main__":
    try: main()
    except (ValueError,OSError,subprocess.SubprocessError,KeyError) as error:
        print("POSITION proof refused:",error,file=sys.stderr)
        raise SystemExit(1)
