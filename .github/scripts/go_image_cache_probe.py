"""Manual read-only cache qualification, not a performance or retention guarantee.

One 900-second work deadline covers four controls; cleanup has one independent
30-second observation deadline. These are context budgets, not OS guarantees.
The committed source inventory is captured once, alongside the separately hashed
output of canonical SDK generation. Only closed progress projections are retained;
raw transport/log/name strings are not replayable from the artifact. Qualification
therefore also trusts this source-bound projector and successful bounded CLI exit.
"""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import sys
import tempfile
import time

from rust_release_evidence import bounded_command
from lake_cache_probe import strict, target_progress

DOCKERFILE = "kanz/services/regulatory/Dockerfile"
SOURCE = "kanz/services/regulatory/cmd/regulatory/main.go"
SDK = "kanz-schemas/gen/go"
MODULE = "kanz/go.mod"
SDK_MODULE = SDK + "/go.mod"
COMMENT = b"\n// Synthetic Go image cache qualification only.\n"
CASES = ("baseline", "source-version", "module", "sdk-module")
BUILDER_IMAGE = "ghcr.io/eighred/base/buildkit:buildx-stable-1"
DOWNLOAD = "sh /usr/local/bin/go-mod-download.sh"
COMPILE = 'CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/eighred/kanz/internal/version.stamped={version}"       -o /out/regulatory ./services/regulatory/cmd/regulatory'
LIMIT = 16 << 20


ERRORS = {"environment", "source", "builder", "command", "build-command-refused", "timeout", "pipe-cleanup", "progress", "cache-execution-state", "cleanup", "seal", "unexpected"}
STAGES = {"initialization", "source", "builder-image", "create", "build", "progress", "cleanup", "seal", "complete"}


class ProbeFailure(ValueError):
    def __init__(self, category):
        if category not in ERRORS: raise ValueError("invalid closed category")
        self.category=category
        super().__init__("cache qualification refused")


def record_failure(state, exc):
    if state["error"] is not None:return
    category = exc.category if isinstance(exc,ProbeFailure) else {"initialization":"environment","source":"source","builder-image":"builder","create":"builder","build":"build-command-refused","progress":"progress","cleanup":"cleanup","seal":"seal"}.get(state["stage"],"unexpected")
    state["error"]=category
    state["failedStage"]=state["stage"]
    state["failedCase"]=state["case"]


def write_status(output, args, state):
    if state["stage"] not in STAGES or state["case"] not in (None,*CASES) or state["error"] not in (None,*ERRORS) or not (state["cleanup"] == "UNKNOWN" or type(state["cleanup"]) is bool):raise ValueError("status projection differs")
    if state["failedStage"] not in (None,*STAGES) or state["failedCase"] not in (None,*CASES):raise ValueError("failed phase projection differs")
    if list(state["completed"]) != list(CASES[:len(state["completed"])]) or len(state["completed"])>4 or any(not re.fullmatch(r"[0-9a-f]{64}",x) for x in state["completed"].values()):raise ValueError("completed case projection differs")
    if state["stage"]=="complete" and (len(state["completed"])!=4 or state["error"] is not None or state["cleanup"] is not True):raise ValueError("incomplete success status")
    body={"schema":1,"source":args.source,"tree":args.tree,"run":args.run,"attempt":args.attempt,
          "stage":state["failedStage"] or state["stage"],"case":state["failedCase"] if state["error"] else state["case"],
          "error":state["error"],"cleanup":state["cleanup"],"passed":state["stage"]=="complete" and state["error"] is None and state["cleanup"] is True,
          "completed":state["completed"]}
    raw=(json.dumps(body,sort_keys=True)+"\n").encode()
    if len(raw)>8192:raise ValueError("status bound exceeded")
    with (output/"status.json").open("xb") as stream:stream.write(raw)


@contextmanager
def owned_workspace(state):
    temporary=tempfile.TemporaryDirectory(prefix="kanz-go-cache-")
    state["builderCleanup"]=True  # no builder has been claimed yet
    try:yield temporary.name
    finally:
        try:temporary.cleanup()
        except BaseException:
            state["cleanup"]="UNKNOWN"
            raise
        state["cleanup"]=state["builderCleanup"]


@contextmanager
def controlled_variant(context, name, source):
    path,version=variant(name,source);original=None
    if path:
        original=(context/path).read_bytes();(context/path).write_bytes(original+COMMENT)
    try:yield path,version
    finally:
        if path:(context/path).write_bytes(original)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def read_file(path, limit=16 << 20):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size > limit:
            raise ValueError("source/evidence is not bounded regular")
        raw = stream.read(limit + 1)
        after = os.fstat(stream.fileno())
        identity = lambda s: (s.st_mode,s.st_nlink,s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns,s.st_ctime_ns)
        if identity(before) != identity(after) or len(raw) != before.st_size:
            raise ValueError("source/evidence changed")
        return raw


def build_args(builder, context, version):
    return ["docker","buildx","build","--builder",builder,"--target","build",
            "--progress=rawjson","--output=type=cacheonly","--cache-from=type=gha,scope=accounting",
            "--build-arg","VERSION="+version,"--file",str(context/DOCKERFILE),str(context)]


def variant(name, source):
    if name not in CASES: raise ValueError("unknown control")
    path = {"source-version":SOURCE,"module":MODULE,"sdk-module":SDK_MODULE}.get(name)
    version = digest(("go-cache-source-version:"+source).encode())[:40] if name == "source-version" else source
    return path, version


def witnesses(raw, name, source):
    _, version = variant(name, source)
    return target_progress(raw,"build",(("download",DOWNLOAD,name in CASES[:2]),("compile",COMPILE.format(version=version),False)))


def closed_progress(raw, name, source):
    """Validate raw lifecycle first, then discard transport/log/name strings."""
    targets=witnesses(raw,name,source)
    projected=[]
    for line in raw.splitlines():
        event=json.loads(line,object_pairs_hook=strict)
        vertices=[]
        for vertex in event.get("vertexes",[]):
            key=vertex["digest"]
            inputs=vertex.get("inputs",[])
            if not isinstance(inputs,list) or len(inputs)>4096 or any(not isinstance(x,str) or not re.fullmatch(r"sha256:[0-9a-f]{64}",x) for x in inputs):raise ValueError("invalid graph inputs")
            item={"digest":key,"inputs":inputs,"name":vertex["name"] if key in targets.values() else "internal",
                  "cached":vertex.get("cached",False),"errorPresent":bool(vertex.get("error"))}
            for field in ("started","completed"):
                if field in vertex:
                    value=vertex[field]
                    if not isinstance(value,str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})",value):raise ValueError("invalid vertex timestamp")
                    item[field]=value
            vertices.append(item)
        projected.append({"vertexes":vertices})
    encoded=("\n".join(json.dumps(e,sort_keys=True,separators=(",",":")) for e in projected)+"\n").encode()
    projected_witnesses(encoded,name,source)
    return encoded


def projected_witnesses(raw,name,source):
    if not raw or len(raw)>LIMIT or not raw.endswith(b"\n"):raise ValueError("projected progress bound")
    parsed=[];latest={}
    for line in raw.splitlines():
        if len(line)>1<<20:raise ValueError("projected line bound")
        event=json.loads(line,object_pairs_hook=strict)
        if not isinstance(event,dict) or set(event)!={"vertexes"} or not isinstance(event["vertexes"],list) or len(event["vertexes"])>4096:raise ValueError("projected event shape")
        converted=[]
        for vertex in event["vertexes"]:
            if not isinstance(vertex,dict) or set(vertex)-{"digest","inputs","name","cached","errorPresent","started","completed"} or not {"digest","inputs","name","cached","errorPresent"}<=vertex.keys() or vertex["errorPresent"] is not False:raise ValueError("projected error/shape")
            if not isinstance(vertex["digest"],str) or not re.fullmatch(r"sha256:[0-9a-f]{64}",vertex["digest"]) or not isinstance(vertex["name"],str) or type(vertex["cached"]) is not bool:raise ValueError("projected identity shape")
            if not isinstance(vertex["inputs"],list) or len(vertex["inputs"])>4096 or any(not isinstance(x,str) or not re.fullmatch(r"sha256:[0-9a-f]{64}",x) for x in vertex["inputs"]):raise ValueError("projected graph shape")
            _,version=variant(name,source)
            if vertex["name"]!="internal" and not any(re.fullmatch(r"\[build [1-9][0-9]*/[1-9][0-9]*\] RUN "+re.escape(cmd),vertex["name"]) for cmd in (DOWNLOAD,COMPILE.format(version=version))):raise ValueError("unprojected vertex name")
            for field in ("started","completed"):
                if field in vertex and (not isinstance(vertex[field],str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})",vertex[field])):raise ValueError("projected timestamp shape")
            item={k:v for k,v in vertex.items() if k!="errorPresent"};converted.append(item);latest[vertex["digest"]]=item
        parsed.append({"vertexes":converted})
        if len(parsed)>20000 or len(latest)>4096:raise ValueError("projected event inventory bound")
    # Progress is an observation stream, not a complete internal solve graph.
    # BuildKit's optional timestamps and cache-local inputs can remain unseen.
    targets=witnesses(("\n".join(json.dumps(e) for e in parsed)+"\n").encode(),name,source)
    return {**targets,"globalCompletion":"not-asserted","observedVertices":len(latest),
            "internalIncomplete":sum(not v.get("started") or not v.get("completed") for key,v in latest.items() if key not in targets.values()),
            "unobservedInputs":sum(i not in latest for v in latest.values() for i in v["inputs"])}


def projection(inventory):
    return digest(json.dumps(inventory,sort_keys=True,separators=(",",":")).encode())


def validate(report, logs, source, tree, run, attempt, inventory, builder_image, builder_ref):
    if not re.fullmatch(r"[0-9a-f]{40}",source) or not re.fullmatch(r"[0-9a-f]{40}",tree): raise ValueError("invalid source")
    if not re.fullmatch(r"[1-9][0-9]*",run) or not re.fullmatch(r"[1-9][0-9]*",attempt): raise ValueError("invalid run")
    expected = {"schema":1,"source":source,"tree":tree,"run":run,"attempt":attempt,
                "contextSHA256":projection(inventory),"dockerfileSHA256":inventory[DOCKERFILE]["sha256"],
                "builderImage":builder_image,"builderRepositoryDigest":builder_ref,"cacheScope":"accounting","readOnly":True,"cleanupPassed":True,"normalContextUnchanged":True}
    if set(report) != set(expected)|{"cases"} or any(report[k] != v for k,v in expected.items()): raise ValueError("receipt identity differs")
    if report["normalContextUnchanged"] is not True or report["readOnly"] is not True or report["cleanupPassed"] is not True or type(report["schema"]) is not int: raise ValueError("receipt verdict differs")
    if not re.fullmatch(r"ghcr.io/eighred/base/buildkit@sha256:[0-9a-f]{64}",builder_ref):raise ValueError("builder repository digest differs")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}",builder_image): raise ValueError("missing actual builder image")
    if set(logs) != set(CASES) or len(report["cases"]) != 4: raise ValueError("control inventory differs")
    builders = set()
    for name,item in zip(CASES,report["cases"]):
        path,version=variant(name,source)
        expected_case={"case":name,"versionSHA256":digest(version.encode()),"changedPath":path,
                       "beforeSHA256":inventory[path]["sha256"] if path else None,
                       "afterSHA256":inventory[path]["commentSHA256"] if path else None,
                       "progressSHA256":digest(logs[name]),**projected_witnesses(logs[name],name,source)}
        if any(type(item.get(field)) is not int for field in ("observedVertices","internalIncomplete","unobservedInputs")):raise ValueError("observation count type differs")
        if set(item)!=set(expected_case)|{"builder"} or any(item[k]!=v for k,v in expected_case.items()): raise ValueError("control receipt differs")
        if not re.fullmatch(r"kanz-go-cache-[0-9a-f]{24}-[0-3]",item["builder"]) or item["builder"] in builders: raise ValueError("fresh builder identity differs")
        builders.add(item["builder"])


class CleanupFailure(ValueError):
    def __init__(self, verdict):
        self.verdict=verdict
        super().__init__("builder cleanup unconfirmed")


def cleanup_builders(command,intents):
    """Only pre-registered fresh names are owned, including lost create replies."""
    if len(intents)>4 or len(set(intents))!=len(intents) or any(not re.fullmatch(r"kanz-go-cache-[0-9a-f]{24}-[0-3]",x) for x in intents):raise ValueError("invalid cleanup ownership")
    failed=False;residue=False
    for builder in reversed(intents):
        try:
            names=command(["docker","buildx","ls","--format","{{.Name}}"],65536,cleanup=True).decode().splitlines()
            if builder in names:command(["docker","buildx","rm","--force",builder],65536,cleanup=True)
        except (ValueError,OSError):failed=True
    for builder in intents:
        for argv in (["docker","buildx","ls","--format","{{.Name}}"],
                     ["docker","ps","-a","--filter","name=^/buildx_buildkit_"+builder+"0$","--format","{{.Names}}"],
                     ["docker","volume","ls","--filter","name=^buildx_buildkit_"+builder+"0_state$","--format","{{.Name}}"]):
            try:
                raw=command(argv,65536,cleanup=True)
                if argv[1]=="buildx":
                    if builder in raw.decode().splitlines():failed=True;residue=True
                elif raw.strip():failed=True;residue=True
            except (ValueError,OSError):failed=True
    if failed:raise CleanupFailure(False if residue else "UNKNOWN")


def execute(args, output, state):
    if sys.platform != "linux" or not all(hasattr(os,x) for x in ("O_NOFOLLOW","O_NONBLOCK")): raise ProbeFailure("environment")
    if not os.environ.get("ACTIONS_RUNTIME_TOKEN") or not os.environ.get("ACTIONS_RESULTS_URL") or os.environ.get("ACTIONS_CACHE_MODE") != "read":raise ProbeFailure("environment")
    root=Path.cwd()
    deadline=time.monotonic()+900; cleanup_deadline=None
    with owned_workspace(state) as owned:
        owned=Path(owned); context=owned/"context";context.mkdir(); counter=0
        def command(argv, limit=1<<20, capture=False, cleanup=False):
            nonlocal counter
            remaining=(cleanup_deadline if cleanup else deadline)-time.monotonic()
            if remaining<=0: raise ProbeFailure("timeout")
            counter+=1; destination=owned/("command-"+str(counter))
            try:bounded_command(argv,destination,limit,timeout=remaining,capture_stderr=capture)
            except ValueError as exc:
                category="timeout" if str(exc)=="image tool exceeded work budget" else "pipe-cleanup" if str(exc)=="tool pipe cleanup exceeded observation budget" else "build-command-refused" if state["stage"]=="build" else "command"
                raise ProbeFailure(category) from None
            except OSError:raise ProbeFailure("command") from None
            raw=read_file(destination,limit);destination.unlink();return raw
        def git(*parts): return command(["git",*parts],2<<20)
        state["stage"]="source"
        if git("rev-parse","HEAD","HEAD^{tree}").decode().splitlines()!=[args.source,args.tree]: raise ValueError("checkout differs")
        if git("status","--porcelain","--untracked-files=no","--","kanz","kanz-schemas/proto",".dockerignore"): raise ValueError("tracked build source dirty")
        entries=git("ls-tree","-r","-l","-z",args.source,"--","kanz",".dockerignore").split(b"\0")
        inventory={};total=0
        def capture(name,mode,blob=None,length=None):
            nonlocal total
            if time.monotonic()>=deadline:raise ValueError("context budget expired")
            path=Path(name)
            if path.is_absolute() or ".." in path.parts or mode not in ("100644","100755") or name in inventory: raise ValueError("source path differs")
            if any((root/p).is_symlink() for p in path.parents): raise ValueError("source parent link")
            raw=read_file(root/path);total+=len(raw)
            if total>512<<20 or len(inventory)>=30000: raise ValueError("context capacity exceeded")
            if blob is not None and (len(raw)!=length or hashlib.sha1(b"blob "+str(len(raw)).encode()+b"\0"+raw).hexdigest()!=blob): raise ValueError("source differs from committed blob")
            target=context/path;target.parent.mkdir(parents=True,exist_ok=True)
            with target.open("xb") as stream:stream.write(raw)
            target.chmod(int(mode[-3:],8))
            inventory[name]={"mode":mode,"blob":blob,"sha256":digest(raw)}
            if name in (SOURCE,MODULE,SDK_MODULE):inventory[name]["commentSHA256"]=digest(raw+COMMENT)
        for entry in entries:
            if not entry:continue
            header,name=entry.decode().split("\t",1);mode,kind,blob,length=header.split()
            if kind!="blob" or not length.isdecimal() or int(length)>16<<20:raise ValueError("unsupported source entry")
            capture(name,mode,blob,int(length))
        # Generated SDK is one separately identified output of the unchanged pinned generation steps.
        sdk=root/SDK
        if not sdk.is_dir() or sdk.is_symlink():raise ValueError("generated SDK missing")
        directory_count=0
        for directory,dirs,files in os.walk(sdk,followlinks=False):
            directory_count+=1
            if directory_count>4096 or time.monotonic()>=deadline:raise ValueError("SDK traversal bound exceeded")
            if any((Path(directory)/d).is_symlink() for d in dirs):raise ValueError("SDK directory link")
            for filename in sorted(files):
                path=Path(directory)/filename;capture(path.relative_to(root).as_posix(),"100644")
        if not {DOCKERFILE,SOURCE,MODULE,SDK_MODULE,".dockerignore"}<=inventory.keys():raise ValueError("incomplete context")
        dockerfile=(context/DOCKERFILE).read_text()
        for part in ("RUN "+DOWNLOAD,"COPY kanz-schemas/gen/go/ /src/kanz-schemas/gen/go/","COPY kanz/ ./","-o /out/regulatory ./services/regulatory/cmd/regulatory"):
            if dockerfile.count(part)!=1:raise ValueError("consumer recipe changed")
        state["stage"]="builder-image"
        image=json.loads(command(["docker","image","inspect",BUILDER_IMAGE],1<<20),object_pairs_hook=strict)
        if len(image)!=1 or image[0].get("Os")!="linux" or image[0].get("Architecture")!="amd64":raise ValueError("builder image differs")
        image_id=image[0]["Id"]
        refs=[ref for ref in image[0].get("RepoDigests",[]) if re.fullmatch(r"ghcr.io/eighred/base/buildkit@sha256:[0-9a-f]{64}",ref)]
        if len(refs)!=1:raise ValueError("builder repository digest missing")
        builder_ref=refs[0]
        nonce=secrets.token_hex(12);results=[];logs={};intents=[]
        def names(cleanup=False):return command(["docker","buildx","ls","--format","{{.Name}}"],65536,cleanup=cleanup).decode().splitlines()
        try:
            for index,name in enumerate(CASES):
                state["stage"]="create";state["case"]=name
                builder="kanz-go-cache-"+nonce+"-"+str(index)
                if builder in names():raise ValueError("builder collision")
                intents.append(builder)
                state["builderCleanup"]="UNKNOWN"
                command(["docker","buildx","create","--name",builder,"--driver","docker-container","--driver-opt","image="+builder_ref])
                state["stage"]="build"
                with controlled_variant(context,name,args.source) as (path,version):
                    raw=command(build_args(builder,context,version),LIMIT,True)
                state["stage"]="progress"
                try:raw=closed_progress(raw,name,args.source)
                except ValueError as exc:
                    raise ProbeFailure("cache-execution-state" if str(exc)=="build vertex did not prove required execution/cache state" else "progress") from None
                target=output/(name+".jsonl")
                try:
                    with target.open("xb") as stream:stream.write(raw)
                except OSError:
                    if target.exists():target.unlink()
                    raise
                state["completed"][name]=digest(raw)
                logs[name]=raw
                results.append({"case":name,"builder":builder,"versionSHA256":digest(version.encode()),"changedPath":path,
                    "beforeSHA256":inventory[path]["sha256"] if path else None,"afterSHA256":inventory[path]["commentSHA256"] if path else None,
                    "progressSHA256":digest(raw),**projected_witnesses(raw,name,args.source)})
        except BaseException as exc:
            record_failure(state,exc)
            raise
        finally:
            state["stage"]="cleanup"
            cleanup_deadline=time.monotonic()+30
            try:cleanup_builders(command,intents)
            except BaseException as exc:
                state["builderCleanup"]=exc.verdict if isinstance(exc,CleanupFailure) else "UNKNOWN"
                raise
            state["builderCleanup"]=True
        state["stage"]="seal"
        if git("status","--porcelain","--untracked-files=no","--","kanz","kanz-schemas/proto",".dockerignore"):raise ProbeFailure("source")
        for name in (SOURCE,MODULE,SDK_MODULE,DOCKERFILE,".dockerignore"):
            if digest(read_file(root/name))!=inventory[name]["sha256"]:raise ProbeFailure("source")
        report={"schema":1,"source":args.source,"tree":args.tree,"run":args.run,"attempt":args.attempt,
                "contextSHA256":projection(inventory),"dockerfileSHA256":inventory[DOCKERFILE]["sha256"],"builderImage":image_id,
                "builderRepositoryDigest":builder_ref,"cacheScope":"accounting","readOnly":True,"cleanupPassed":True,"normalContextUnchanged":True,"cases":results}
        validate(report,logs,args.source,args.tree,args.run,args.attempt,inventory,image_id,builder_ref)
    (output/"inventory.json").write_text(json.dumps(inventory,sort_keys=True)+"\n",encoding="utf-8")
    (output/"proof.json").write_text(json.dumps(report,sort_keys=True)+"\n",encoding="utf-8")
    state["stage"]="complete";state["case"]=None
    print("Go shared-cache controls: four compiled cases and owned cleanup verified")


def main():
    parser=argparse.ArgumentParser()
    for name in ("source","tree","run","attempt","output"):parser.add_argument("--"+name,required=True)
    args=parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}",args.source) or not re.fullmatch(r"[0-9a-f]{40}",args.tree) or not re.fullmatch(r"[1-9][0-9]*",args.run) or not re.fullmatch(r"[1-9][0-9]*",args.attempt):raise SystemExit("invalid fixed qualification identity")
    output=Path(args.output);output.mkdir(exist_ok=False)
    state={"stage":"initialization","case":None,"error":None,"failedStage":None,"failedCase":None,"cleanup":"UNKNOWN","completed":{}}
    failed=False
    try:execute(args,output,state)
    except BaseException as exc:
        record_failure(state,exc);failed=True
    finally:write_status(output,args,state)
    if failed:raise SystemExit("Go cache qualification refused; inspect closed status receipt")


if __name__=="__main__":main()
