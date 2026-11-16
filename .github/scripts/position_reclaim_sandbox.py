"""Narrow Linux execution boundary for the disposable #1014 CI proof.

The parent owns Docker, namespace setup and artifact promotion. Target processes
get a read-only source copy, public tools/dependencies, private loopback and only
the fixed CI backend relays below. There is no generic forwarding request format.

Execution requires prior review of the exact checked head of kanz-ci.yml and
both privileged parent programs, position_reclaim_proof.py and this module.
Child restrictions cannot contain a modified workflow or either parent program.
"""
import contextlib
import errno
import hashlib
import inspect
import json
import os
from pathlib import Path
import selectors
import shutil
import socket
import stat
import subprocess
import sys
import threading
import time
import uuid

PORTS = (4222, 4223, 4224, 4225, 5432, 6379, 9092)
# The outer sockets must not reuse Docker's wildcard-published backend ports.
# Child loopback endpoints remain canonical; only this fixed transit leg differs.
RELAY_PORTS = ((24222,4222),(24223,4223),(24224,4224),(24225,4225),
               (25432,5432),(26379,6379),(39092,9092))
UID = 65534
MAX_FILE = 32 * 1024 * 1024
MAX_SOURCE = 1024 * 1024 * 1024
MAX_OUTPUT = 256 * 1024 * 1024
MAX_GIT_METADATA_ENTRIES = 10000
SAFE = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8", "GIT_CONFIG_NOSYSTEM":"1", "GIT_CONFIG_GLOBAL":"/dev/null"}
PROBE_ENTRY = b"POSITION_PROBE_ENTRY v1"
PROBE_STAGES = frozenset(("arguments","identity","uid-map","gid-map","environment","core-limit","file-limit",
                         "capabilities","hidden-paths","input-readonly","write-boundary","root-readonly","device",
                         "scratch-write","private-loopback","fixed-backends","egress-refusal","proc-isolation",
                         "success-record","sampling-handshake"))
PROBE_REFUSALS = frozenset(("assertion","os-operation","unexpected"))
PROBE_ENVIRONMENT = b"POSITION_PROBE_ENVIRONMENT v1 "
WORKING_DIRECTORIES = frozenset(("/input", "/input/kanz", "/input/rust"))
# Exact English marker observations only, not an inferred first cause. Verified
# against bubblewrap v0.9.0 bubblewrap.c and util-linux v2.39.3 prlimit.c/c.h.
# Other versions/messages remain unknown; these markers grant no recovery action.
LAUNCHER_REFUSALS = {
    b"bwrap: Creating new namespace failed: Operation not permitted":"bwrap-namespace-refused",
    b"bwrap: No permissions to creating new namespace, likely because the kernel does not allow non-privileged user namespaces. On e.g. debian this can be enabled with 'sysctl kernel.unprivileged_userns_clone=1'.":"bwrap-namespace-refused",
    b"prlimit: failed to execute python3: Permission denied":"prlimit-exec-refused",
    b"prlimit: failed to execute python3: Operation not permitted":"prlimit-exec-refused",
}


def launch_observation(supervisor_exit, raw):
    """Never retain an unknown launcher error, environment value or its hash."""
    if type(supervisor_exit) is not int or not -255<=supervisor_exit<=255 or type(raw) is not bytes:
        raise ValueError("launcher observation process metadata refused")
    window=raw[:4096]
    if len(raw)>4096 and not window.endswith(b"\n"):
        window=window.rsplit(b"\n",1)[0] if b"\n" in window else b""
    markers={LAUNCHER_REFUSALS[line] for line in window.splitlines() if line in LAUNCHER_REFUSALS} if supervisor_exit else set()
    return {"schema":1,"supervisor_exit":supervisor_exit,"combined_output_bytes":len(raw),
            "launcher_refusal":next(iter(markers)) if len(markers)==1 else "unknown"}


def effective_environment(base, cwd, extra=None):
    # bubblewrap supplies PWD for --chdir. Declare that public value ourselves;
    # neither an ambient value nor an extra assignment may choose it.
    if cwd not in WORKING_DIRECTORIES or "PWD" in base or (extra and "PWD" in extra):
        raise ValueError("working-directory environment refused")
    result = dict(base)
    if extra:
        result.update(extra)
    result["PWD"] = cwd
    return result


def probe_environment_output(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate environment observation field")
            result[key] = value
        return result
    if len(raw) > 2048 or not raw.startswith(PROBE_ENVIRONMENT):
        raise ValueError("environment observation frame refused")
    value = json.loads(raw[len(PROBE_ENVIRONMENT):], object_pairs_hook=unique)
    counts = ("expected_count", "observed_count", "missing_count", "unexpected_count", "unknown_count")
    if (not isinstance(value, dict) or set(value) != {"schema", *counts, "known_unexpected", "pwd_matches_cwd"}
            or type(value["schema"]) is not int or value["schema"] != 1
            or any(type(value[key]) is not int or not 0 <= value[key] <= 128 for key in counts)
            or type(value["pwd_matches_cwd"]) is not bool):
        raise ValueError("environment observation fields refused")
    known = value["known_unexpected"]
    if (not isinstance(known, list) or any(type(key) is not str or key not in {"PWD", "LC_CTYPE", "SHLVL", "_"} for key in known)
            or known != sorted(set(known)) or not 1 <= value["expected_count"]
            or value["missing_count"] > value["expected_count"] or value["unexpected_count"] > value["observed_count"]
            or value["observed_count"] != value["expected_count"] - value["missing_count"] + value["unexpected_count"]
            or value["unexpected_count"] != len(known) + value["unknown_count"]
            or not (value["missing_count"] or value["unexpected_count"] or not value["pwd_matches_cwd"])):
        raise ValueError("environment observation counts refused")
    return value


def probe_output(supervisor_exit, raw, combined_output_bytes):
    """Project a bounded fixed child protocol; return no unknown diagnostic text."""
    observation={"schema":1,"supervisor_exit":supervisor_exit,"combined_output_bytes":combined_output_bytes,
                 "inner_entry":False,"framing":"unrecognized","stage":"unknown","refusal":"unknown",
                 "launcher_refusal":"unknown"}
    if (type(supervisor_exit) is not int or not -255<=supervisor_exit<=255 or type(raw) is not bytes
            or type(combined_output_bytes) is not int or not len(raw)<=combined_output_bytes<=MAX_OUTPUT+65536):
        raise ValueError("probe observation process metadata refused")
    if len(raw)>65536 or combined_output_bytes>65536:
        observation["framing"]="oversize"
        return observation,None
    lines=raw.splitlines()
    if not lines or lines[0]!=PROBE_ENTRY:
        observation["framing"]="missing-entry"
        observation["launcher_refusal"]=launch_observation(supervisor_exit,raw)["launcher_refusal"]
        return observation,None
    observation["inner_entry"]=True
    environment = None
    if len(lines) == 3 and supervisor_exit and lines[1].startswith(PROBE_ENVIRONMENT):
        try:
            environment = probe_environment_output(lines[1])
        except (ValueError, UnicodeError, TypeError):
            observation["framing"] = "invalid-environment"
            return observation, None
        lines = [lines[0], lines[2]]
    if len(lines)!=2 or lines.count(PROBE_ENTRY)!=1:
        observation["framing"]="unexpected-output"
        return observation,None
    if supervisor_exit:
        parts=lines[1].split(b" ")
        if len(parts)==5 and parts[:2]==[b"POSITION_PROBE_REFUSAL",b"v1"]:
            try: stage,refusal=parts[3].decode("ascii"),parts[4].decode("ascii")
            except UnicodeDecodeError: pass
            else:
                if parts[2]==b"stage" and stage in PROBE_STAGES and refusal in PROBE_REFUSALS:
                    if environment is not None:
                        if stage != "environment" or refusal != "assertion":
                            observation["framing"] = "invalid-environment"
                            return observation, None
                        observation["environment_difference"] = environment
                    observation.update(framing="refusal",stage=stage,refusal=refusal)
                    return observation,None
        observation["framing"]="invalid-refusal"
        return observation,None
    try: candidate=json.loads(lines[1])
    except (ValueError,UnicodeError): candidate=None
    if not isinstance(candidate,dict):
        observation["framing"]="invalid-record"
        return observation,None
    observation["framing"]="candidate-record"
    # The caller still performs every original independent JSON/kernel check.
    return observation,lines[1]


def host(*args, timeout=30, input=None, umask=-1):
    process=subprocess.Popen(args,stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,env=SAFE,start_new_session=True,umask=umask)
    result=bytearray()
    until=time.monotonic()+timeout
    try:
        if input is not None:
            if len(input)>1024*1024: raise ValueError("trusted provisioning input exceeds bound")
            os.set_blocking(process.stdin.fileno(),False)
        written=0
        with selectors.DefaultSelector() as poll:
            poll.register(process.stdout,selectors.EVENT_READ)
            if input is not None: poll.register(process.stdin,selectors.EVENT_WRITE)
            while poll.get_map():
                if time.monotonic()>until: raise ValueError("trusted provisioning timeout")
                for key,_ in poll.select(.1):
                    if key.fileobj is process.stdin:
                        try: written+=os.write(key.fd,input[written:written+16384])
                        except BrokenPipeError: written=len(input)
                        if written==len(input):
                            poll.unregister(process.stdin)
                            process.stdin.close()
                        continue
                    chunk=os.read(key.fd,65536)
                    if not chunk: poll.unregister(key.fileobj); continue
                    result.extend(chunk)
                    if len(result)>4*1024*1024: raise ValueError("trusted provisioning output exceeds bound")
        if process.wait(timeout=3): raise subprocess.CalledProcessError(process.returncode,args)
        return bytes(result)
    finally:
        if process.poll() is None:
            import signal
            os.killpg(process.pid,signal.SIGKILL)
        process.wait(timeout=5)
        for stream in (process.stdin,process.stdout):
            if stream is not None: stream.close()


def directory(path):
    """Retain an absolute no-follow component-walk descriptor."""
    path = Path(path)
    if not path.is_absolute() or str(path) != os.path.normpath(path):
        raise ValueError("canonical absolute directory required")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        for component in path.parts[1:]:
            nxt = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
            os.close(fd)
            fd = nxt
        return fd
    except BaseException:
        os.close(fd)
        raise


class EvidenceFileRefusal(ValueError):
    """Bounded metadata only; neither a pathname nor rejected file bytes."""
    def __init__(self, info, limit):
        super().__init__("nonregular, linked or oversized evidence refused")
        regular=stat.S_ISREG(info.st_mode)
        if any(type(n) is not int or not 0<=n<=2**63-1 for n in (info.st_nlink,info.st_size,limit)):
            raise ValueError("evidence file metadata is unrepresentable")
        self.observation={"regular":regular,"link_count":info.st_nlink,"bytes":info.st_size,"limit_bytes":limit,
                          "reason":"nonregular" if not regular else "multiple-links" if info.st_nlink!=1 else "oversized"}


def read_regular(root_fd, relative, limit):
    parts = Path(relative).parts
    if not parts or Path(relative).is_absolute() or any(p in (".", "..") for p in parts):
        raise ValueError("relative regular-file path required")
    fd = os.dup(root_fd)
    try:
        for component in parts[:-1]:
            nxt = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
            os.close(fd)
            fd = nxt
        leaf = os.open(parts[-1], os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
        try:
            before = os.fstat(leaf)
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size > limit:
                raise EvidenceFileRefusal(before,limit)
            chunks, count = [], 0
            while chunk := os.read(leaf, min(65536, limit + 1 - count)):
                chunks.append(chunk)
                count += len(chunk)
                if count > limit:
                    raise ValueError("evidence grew past its bound")
            after = os.fstat(leaf)
            fields = ("st_dev", "st_ino", "st_mode", "st_nlink", "st_size", "st_mtime_ns", "st_ctime_ns")
            if any(getattr(before,k) != getattr(after,k) for k in fields) or count != before.st_size:
                raise ValueError("evidence changed while reading")
            return b"".join(chunks)
        finally:
            os.close(leaf)
    finally:
        os.close(fd)


def new_file(root_fd, name, raw, mode=0o600):
    if Path(name).name != name or name in ("", ".", ".."):
        raise ValueError("one new artifact leaf required")
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, mode, dir_fd=root_fd)
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            raise ValueError("unsafe artifact destination")
        os.fchmod(fd, mode)
        configured=os.fstat(fd)
        if (before.st_dev,before.st_ino)!=(configured.st_dev,configured.st_ino) or not stat.S_ISREG(configured.st_mode) or configured.st_nlink!=1 or stat.S_IMODE(configured.st_mode)!=mode:
            raise ValueError("artifact destination mode differs")
        with os.fdopen(fd, "wb", closefd=False) as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(fd)
        after = os.fstat(fd)
        if (before.st_dev,before.st_ino) != (after.st_dev,after.st_ino) or not stat.S_ISREG(after.st_mode) or after.st_nlink != 1 or after.st_size != len(raw) or stat.S_IMODE(after.st_mode)!=mode:
            raise ValueError("artifact destination changed")
    finally:
        os.close(fd)


def owned_directory_mode(fd, mode):
    before=os.fstat(fd)
    if not stat.S_ISDIR(before.st_mode) or before.st_uid!=os.geteuid():
        raise ValueError("owned staging directory required")
    os.fchmod(fd,mode)
    after=os.fstat(fd)
    if (before.st_dev,before.st_ino)!=(after.st_dev,after.st_ino) or stat.S_IMODE(after.st_mode)!=mode:
        raise ValueError("owned staging directory mode differs")


class VisibleInputCapsule:
    """One initial /run input allocation; never a retry location or artifact root."""
    def __init__(self):
        self.nonce=uuid.uuid4().hex
        self.name="position-reclaim-inputs-"+self.nonce
        self.path=Path("/run")/self.name
        self.root_fd=self.run_fd=self.fd=None
        self.identity=None
        self.created=False
        self.absent=None
        self.stage="unallocated"
        self.ancestors={}

    def ancestor(self, role, fd):
        actual=os.fstat(fd)
        self.ancestors[role]={"role":role,"uid":actual.st_uid,"gid":actual.st_gid,
                              "mode":stat.S_IMODE(actual.st_mode),"directory":stat.S_ISDIR(actual.st_mode)}
        if not stat.S_ISDIR(actual.st_mode) or actual.st_uid!=0 or actual.st_gid!=0 or stat.S_IMODE(actual.st_mode)!=0o755:
            raise ValueError("fixed visible input ancestor refused")
        return actual

    def allocate(self):
        if sys.platform!="linux" or os.geteuid()!=0 or self.stage!="unallocated":
            raise ValueError("one privileged initial capsule allocation required")
        self.declaration()
        self.stage="filesystem-root"
        self.root_fd=directory("/")
        self.ancestor("filesystem-root",self.root_fd)
        self.stage="runtime-parent"
        self.run_fd=os.open("run",os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=self.root_fd)
        self.ancestor("runtime-parent",self.run_fd)
        self.stage="exclusive-allocation"
        # One UUID and one exclusive attempt. Never adopt an existing leaf.
        os.mkdir(self.name,mode=0o700,dir_fd=self.run_fd)
        self.created=True
        actual=os.stat(self.name,dir_fd=self.run_fd,follow_symlinks=False)
        if not stat.S_ISDIR(actual.st_mode) or actual.st_uid!=0 or actual.st_gid!=0:
            raise ValueError("allocated visible input owner refused")
        self.identity=(actual.st_dev,actual.st_ino)
        self.fd=os.open(self.name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=self.run_fd)
        actual=os.fstat(self.fd)
        if not stat.S_ISDIR(actual.st_mode) or actual.st_uid!=0 or actual.st_gid!=0 or (actual.st_dev,actual.st_ino)!=self.identity:
            raise ValueError("allocated visible input owner refused")
        owned_directory_mode(self.fd,0o711)
        self.stage="allocated"
        self.verify()

    def declaration(self):
        if len(self.nonce)!=32 or any(c not in "0123456789abcdef" for c in self.nonce) or self.name!="position-reclaim-inputs-"+self.nonce or self.path!=Path("/run")/self.name:
            raise ValueError("visible input nonce refused")

    def verify(self):
        if self.stage!="allocated" or not self.created or self.fd is None or self.identity is None:
            raise ValueError("allocated visible input custody required")
        self.owned_entry((0o711,))

    def owned_entry(self, modes):
        if not self.created or self.identity is None or self.root_fd is None or self.run_fd is None:
            raise ValueError("visible input entry custody unproven")
        self.declaration()
        self.ancestor("filesystem-root",self.root_fd)
        parent=self.ancestor("runtime-parent",self.run_fd)
        linked_parent=os.stat("run",dir_fd=self.root_fd,follow_symlinks=False)
        if not stat.S_ISDIR(linked_parent.st_mode) or (parent.st_dev,parent.st_ino)!=(linked_parent.st_dev,linked_parent.st_ino):
            raise ValueError("fixed visible input parent changed")
        linked=os.stat(self.name,dir_fd=self.run_fd,follow_symlinks=False)
        checks=(linked,) if self.fd is None else (os.fstat(self.fd),linked)
        for actual in checks:
            if not stat.S_ISDIR(actual.st_mode) or (actual.st_dev,actual.st_ino)!=self.identity or actual.st_uid!=0 or actual.st_gid!=0 or stat.S_IMODE(actual.st_mode) not in modes:
                raise ValueError("visible input capsule changed")

    def metadata(self):
        return {"schema":1,"stage":self.stage,"nonce":self.nonce,"created":self.created,
                "identity":list(self.identity) if self.identity is not None else None,
                "absence_observed":self.absent,"ancestors":[self.ancestors[k] for k in sorted(self.ancestors)]}

    def retire(self, *, resources_absent):
        if resources_absent is not True:
            raise ValueError("capsule retirement requires observed resource teardown")
        if not self.created:
            self.release()
            return # No owned allocation exists; an exclusive collision is never adopted.
        partial=self.stage!="allocated"
        if partial: self.owned_entry((0o700,0o711))
        else: self.verify()
        # Failed initial allocation has created no child tree. Remove only its
        # identity-checked empty directory, without retrying a failed open/chmod.
        entries=[] if partial else os.listdir(self.fd)
        if entries and shutil.rmtree.avoids_symlink_attacks is not True:
            raise ValueError("no-follow capsule retirement unavailable")
        if not set(entries)<={"inputs","scratch"}:
            raise ValueError("undeclared visible input child")
        for name in entries:
            actual=os.stat(name,dir_fd=self.fd,follow_symlinks=False)
            if not stat.S_ISDIR(actual.st_mode) or actual.st_uid!=0 or actual.st_gid!=0 or stat.S_IMODE(actual.st_mode)!={"inputs":0o711,"scratch":0o700}[name]:
                raise ValueError("visible input child owner refused")
        # All target processes and every mount are gone. Python's fd-relative
        # no-follow removal stays beneath the retained, identity-checked capsule.
        for name in entries: shutil.rmtree(name,dir_fd=self.fd)
        self.owned_entry((0o700,0o711) if partial else (0o711,))
        os.rmdir(self.name,dir_fd=self.run_fd)
        try: os.stat(self.name,dir_fd=self.run_fd,follow_symlinks=False)
        except FileNotFoundError: self.absent=True
        else: raise ValueError("retired visible input absence unproven")
        self.stage="retired"
        self.release()

    def release(self):
        for name in ("fd","run_fd","root_fd"):
            fd=getattr(self,name)
            if fd is not None:
                os.close(fd)
                setattr(self,name,None)


def public_parents(root_fd, relative, mode=0o755):
    """Create only owned source-relative parents; never chmod an existing tree."""
    parts=Path(relative).parts
    if Path(relative).is_absolute() or any(p in (".","..") for p in parts):
        raise ValueError("owned relative staging path required")
    fd=os.dup(root_fd)
    try:
        for name in parts:
            created=False
            try:
                os.mkdir(name,mode=mode,dir_fd=fd)
                created=True
            except FileExistsError: pass
            child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=fd)
            try:
                if created: owned_directory_mode(child,mode)
                actual=os.fstat(child)
                if actual.st_uid!=os.geteuid() or stat.S_IMODE(actual.st_mode)!=mode:
                    raise ValueError("owned public staging mode refused")
            except BaseException:
                os.close(child)
                raise
            os.close(fd);fd=child
        return fd
    except BaseException:
        os.close(fd)
        raise


def check_git_metadata(source_fd):
    """Read-only, bounded no-follow validation of newly generated metadata."""
    count=total=0
    def visit(fd):
        nonlocal count,total
        actual=os.fstat(fd)
        if not stat.S_ISDIR(actual.st_mode) or actual.st_uid!=os.geteuid() or stat.S_IMODE(actual.st_mode)!=0o755:
            raise ValueError("generated Git directory mode refused")
        with os.scandir(fd) as entries:
            for entry in entries:
                name=entry.name
                count+=1
                if count>MAX_GIT_METADATA_ENTRIES: raise ValueError("generated Git metadata count refused")
                before=os.stat(name,dir_fd=fd,follow_symlinks=False)
                if stat.S_ISDIR(before.st_mode):
                    child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=fd)
                    try: visit(child)
                    finally: os.close(child)
                else:
                    child=os.open(name,os.O_RDONLY|os.O_NONBLOCK|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=fd)
                    try:
                        actual=os.fstat(child)
                        total+=actual.st_size
                        if (not stat.S_ISREG(actual.st_mode) or actual.st_uid!=os.geteuid() or actual.st_nlink!=1
                                or stat.S_IMODE(actual.st_mode) not in (0o644,0o444) or actual.st_size>MAX_FILE or total>MAX_SOURCE):
                            raise ValueError("generated Git file boundary refused")
                    finally: os.close(child)
    fd=os.open(".git",os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC,dir_fd=source_fd)
    try: visit(fd)
    finally: os.close(fd)


def launch_permissions(specs):
    """Trusted outer-identity access checks; never return a path or error text."""
    roles={"filesystem-root","runtime-parent","assigned-root","input-parent","public-parent","source","git-metadata","synthetic-identity","readonly-mount","scratch"}
    if not isinstance(specs,list) or not 1<=len(specs)<=64:
        raise ValueError("launch input declarations refused")
    rows=[]
    for index,spec in enumerate(specs):
        if (not isinstance(spec,list) or len(spec)!=5 or spec[0] not in roles
                or not isinstance(spec[1],str) or not os.path.isabs(spec[1]) or os.path.normpath(spec[1])!=spec[1]
                or spec[2] not in ("directory","file") or (spec[3] is not None and (type(spec[3]) is not int or not 0<=spec[3]<=0o777))
                or spec[4] not in (os.R_OK,os.X_OK,os.R_OK|os.X_OK)):
            raise ValueError("launch input declaration refused")
        role,path,kind,mode,access=spec
        row={"index":index,"role":role,"accessible":False,"mode_ok":False,"errno":None}
        try:
            actual=os.stat(path)
            directory=stat.S_ISDIR(actual.st_mode)
            row["mode_ok"]=(directory if kind=="directory" else stat.S_ISREG(actual.st_mode)) and (mode is None or stat.S_IMODE(actual.st_mode)==mode)
            if os.access(path,access,effective_ids=True):
                flags=os.O_CLOEXEC|os.O_NONBLOCK
                flags|=os.O_PATH|os.O_DIRECTORY if directory and access==os.X_OK else os.O_RDONLY|(os.O_DIRECTORY if directory else 0)
                fd=os.open(path,flags)
                try: row.update(accessible=True,errno=0)
                finally: os.close(fd)
        except OSError as error:
            row["errno"]=error.errno if type(error.errno) is int and 0<error.errno<=4095 else None
        rows.append(row)
    return {"schema":1,"status":"passed" if all(r["accessible"] and r["mode_ok"] and r["errno"]==0 for r in rows) else "refused","rows":rows}


def launch_permission_output(raw, roles):
    if type(raw) is not bytes or len(raw)>16384 or not raw.startswith(b"POSITION_LAUNCH_ACCESS "):
        raise ValueError("launch permission frame refused")
    try: result=json.loads(raw.removeprefix(b"POSITION_LAUNCH_ACCESS "))
    except (ValueError,UnicodeError): raise ValueError("launch permission frame refused") from None
    if not isinstance(result,dict) or set(result)!={"schema","status","rows"} or type(result["schema"]) is not int or result["schema"]!=1 or not isinstance(result["rows"],list) or len(result["rows"])!=len(roles):
        raise ValueError("launch permission frame identity refused")
    for index,(row,role) in enumerate(zip(result["rows"],roles)):
        if (not isinstance(row,dict) or set(row)!={"index","role","accessible","mode_ok","errno"}
                or type(row["index"]) is not int or row["index"]!=index or row["role"]!=role
                or type(row["accessible"]) is not bool or type(row["mode_ok"]) is not bool
                or (row["errno"] is not None and (type(row["errno"]) is not int or not 0<=row["errno"]<=4095))
                or (row["accessible"] and row["errno"]!=0) or (not row["accessible"] and row["errno"]==0)):
            raise ValueError("launch permission row refused")
    expected="passed" if all(r["accessible"] and r["mode_ok"] and r["errno"]==0 for r in result["rows"]) else "refused"
    if result["status"]!=expected: raise ValueError("launch permission status refused")
    return result


class FixedRelay:
    """Byte transport to one compile-time endpoint, bounded to 96 connections."""
    def __init__(self, listen, upstream, routes):
        self.stop = threading.Event()
        self.lock = threading.Lock()
        self.sockets = set()
        self.threads = []
        self.workers = set()
        self.slots = threading.BoundedSemaphore(96)
        self.listeners = []
        try:
            # Prebind every owned socket before starting any worker. A later
            # collision cannot leave a partially constructed relay running.
            for port,_ in routes:
                listener = socket.socket()
                self.listeners.append(listener)
                listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                listener.bind((listen, port))
                listener.listen(32)
                listener.settimeout(.25)
            for listener,(_,port) in zip(self.listeners,routes):
                thread = threading.Thread(target=self.accept, args=(listener,upstream,port))
                thread.start()
                self.threads.append(thread)
        except BaseException as primary:
            try: self.close()
            except BaseException as cleanup:
                primary.position_relay_cleanup_refused = True
                primary.add_note("fixed relay cleanup also refused: "+type(cleanup).__name__)
                print("POSITION relay cleanup also refused:",type(cleanup).__name__,file=sys.stderr)
            raise

    def accept(self, listener, upstream, port):
        while not self.stop.is_set():
            try:
                client,_ = listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            if not self.slots.acquire(blocking=False):
                client.close()
                continue
            with self.lock:
                self.sockets.add(client)
            thread = threading.Thread(target=self.transfer,args=(client,upstream,port))
            with self.lock: self.workers.add(thread)
            thread.start()

    def transfer(self, client, upstream, port):
        server = None
        try:
            server = socket.create_connection((upstream,port),timeout=5)
            with self.lock:
                self.sockets.add(server)
            for s in (client,server): s.setblocking(False)
            # Small bounded buffers and half-close handling preserve long-lived
            # Kafka/NATS/PG sessions without a CONNECT or user-selected address.
            buffers = {client:bytearray(),server:bytearray()}
            peers = {client:server,server:client}
            reading = {client,server}
            while not self.stop.is_set() and (reading or any(buffers.values())):
                with selectors.DefaultSelector() as poll:
                    for s in (client,server):
                        mask = (selectors.EVENT_READ if s in reading and len(buffers[peers[s]])<65536 else 0) | (selectors.EVENT_WRITE if buffers[s] else 0)
                        if mask: poll.register(s,mask)
                    for key,mask in poll.select(.25):
                        s = key.fileobj
                        if mask & selectors.EVENT_READ:
                            data = s.recv(16384)
                            if data: buffers[peers[s]].extend(data)
                            else: reading.remove(s)
                        if mask & selectors.EVENT_WRITE:
                            sent = s.send(buffers[s])
                            del buffers[s][:sent]
                for s in tuple(reading):
                    if peers[s] not in reading and not buffers[s]:
                        with contextlib.suppress(OSError): s.shutdown(socket.SHUT_WR)
        except OSError:
            pass  # The actual client reports connection failure without endpoint diagnostics.
        finally:
            with self.lock:
                for s in (client,server):
                    if s is not None:
                        self.sockets.discard(s)
                        s.close()
            self.slots.release()
            with self.lock: self.workers.discard(threading.current_thread())

    def close(self):
        self.stop.set()
        failures=[]
        for s in self.listeners:
            try: s.close()
            except OSError as error: failures.append(type(error).__name__)
        # Stop acceptors before taking the worker snapshot, so no new worker
        # can escape the subsequent shutdown/join check.
        for thread in self.threads: thread.join(6)
        with self.lock:
            for s in tuple(self.sockets):
                with contextlib.suppress(OSError): s.shutdown(socket.SHUT_RDWR)
        with self.lock: workers=list(self.workers)
        for thread in workers: thread.join(6)
        if failures or any(t.is_alive() for t in self.threads+workers):
            raise ValueError("fixed relay did not stop/join")


class Sandbox:
    def __init__(self, repo, *, capsule, artifact_root, tools, env):
        if sys.platform != "linux" or os.geteuid()!=0:
            raise ValueError("Linux root parent orchestration required")
        if not isinstance(capsule,VisibleInputCapsule):
            raise ValueError("explicit visible input capsule required")
        self.capsule=capsule
        self.repo, self.root, self.tools = Path(repo), capsule.path, tools
        self.artifact_root=Path(artifact_root)
        if not self.artifact_root.is_absolute() or self.artifact_root.is_relative_to(self.root):
            raise ValueError("private artifacts must be outside visible inputs")
        self.artifact_root_canonical=self.artifact_root.resolve(strict=True)
        if self.artifact_root_canonical.is_relative_to(self.root):
            raise ValueError("private artifact alias enters visible inputs")
        self.environment = env
        self.suffix = uuid.uuid4().hex[:10]
        self.namespace = "position-"+self.suffix
        self.interface = "pr"+self.suffix
        # No route leaves this /30. Firewall also refuses host destinations
        # other than the seven fixed transit ports (24225 reaches the negative broker).
        self.host_ip,self.peer_ip = "10.239.254.1","10.239.254.2"
        self.work = self.root/"scratch"
        self.inputs = self.root/"inputs"
        self.out = self.artifact_root/"artifacts"
        self.active = set()
        self.relay = None
        self.relay_constructor_cleanup_refused = False
        self.inner = None
        self.mounted = self.net_created = self.veth_created = False
        self.work_fd = self.out_fd = self.receipts_fd = None
        self.retained = 0
        self.artifacts = {}
        self.cleanup_verified = False
        self.capsule_cleanup_complete = False
        self.executions = []
        self.public_mounts = {}

    def start(self):
        for command in ("bwrap","setpriv","systemd-run","ip","iptables","ip6tables","mount","umount","prlimit"):
            if not shutil.which(command,path=SAFE["PATH"]): raise ValueError("missing enforced isolation tool: "+command)
        private_fd=directory(self.artifact_root)
        try:
            private=os.fstat(private_fd)
            if not stat.S_ISDIR(private.st_mode) or private.st_uid!=os.geteuid() or stat.S_IMODE(private.st_mode)!=0o700:
                raise ValueError("private artifact root refused")
        finally: os.close(private_fd)
        self.out.mkdir(mode=0o700)
        self.out_fd = directory(self.out)
        self.receipts_fd = os.dup(self.out_fd)
        try: self.capsule.allocate()
        finally: self.retain("input-capsule.json",json.dumps(self.capsule.metadata(),sort_keys=True).encode())
        for path in (self.work,self.inputs): path.mkdir(mode=0o700)
        for path in (self.inputs,):
            fd=directory(path)
            try: owned_directory_mode(fd,0o711)
            finally: os.close(fd)
        input_fd=directory(self.inputs)
        try:
            fd=public_parents(input_fd,"public",mode=0o711)
            os.close(fd)
        finally: os.close(input_fd)
        if stat.S_IMODE(os.fstat(self.out_fd).st_mode)!=0o700:
            raise ValueError("private artifact directory mode refused")
        host("mount","-t","tmpfs","-o","size=5G,nodev,nosuid,mode=0700,uid=65534,gid=65534","position-proof",str(self.work))
        self.mounted = True
        self.work_fd = directory(self.work)
        for name in ("tmp","home","cache","cargo","rust-target"):
            (self.work/name).mkdir(mode=0o700)
            os.chown(self.work/name,UID,UID)
        # Source copy is parent-owned and never writable by target processes.
        source = self.inputs/"source"
        input_fd=directory(self.inputs)
        try: source_fd=public_parents(input_fd,"source")
        finally: os.close(input_fd)
        repo_fd = directory(self.repo)
        total = 0
        try:
            names = host("git","-c","safe.directory="+str(self.repo),"-C",str(self.repo),"ls-files","-z").split(b"\0")
            for raw in names:
                if not raw: continue
                name = os.fsdecode(raw)
                data = read_regular(repo_fd,name,MAX_FILE)
                total += len(data)
                if total > MAX_SOURCE: raise ValueError("source copy exceeds bound")
                dest = Path(name)
                parent_fd=public_parents(source_fd,str(dest.parent))
                try:
                    # Preserve executable mode only on this exclusively created leaf.
                    new_file(parent_fd,dest.name,data,mode=0o755 if (self.repo/name).stat().st_mode & 0o111 else 0o644)
                finally: os.close(parent_fd)
        finally:
            os.close(repo_fd)
            os.close(source_fd)
        source_fd=directory(source)
        try:
            for name in ("schemas/gen/go","web/node_modules","web/dist"):
                fd=public_parents(source_fd,name)
                os.close(fd)
        finally: os.close(source_fd)
        # A private exact-head Git object/index copy preserves VCS and index
        # tests without exposing the checkout's config, auth header or worktree.
        source_sha=host("git","-c","safe.directory="+str(self.repo),"-C",str(self.repo),"rev-parse","HEAD").decode().strip()
        host("git","init","--template=","--quiet",str(source),umask=0o022)
        host("git","-c","safe.directory="+str(self.repo),"-c","core.hooksPath=/dev/null","-C",str(source),"fetch","--quiet","--no-tags","--depth=1",str(self.repo),source_sha,timeout=90,umask=0o022)
        host("git","-C",str(source),"reset","--quiet","--mixed",source_sha,umask=0o022)
        (source/".git/FETCH_HEAD").unlink()
        source_fd=directory(source)
        try: check_git_metadata(source_fd)
        finally: os.close(source_fd)
        # A real sysroot is read-only; these synthetic identity files contain no
        # host accounts and make user lookup deterministic inside the boundary.
        input_fd=directory(self.inputs)
        try:
            new_file(input_fd,"passwd",b"nobody:x:65534:65534:fixture:/scratch/home:/usr/sbin/nologin\n",mode=0o644)
            new_file(input_fd,"group",b"nogroup:x:65534:\n",mode=0o644)
        finally: os.close(input_fd)
        # No .git/config, ambient home, Docker socket or Actions credentials are
        # copied. Generated contracts and browser inputs are separate ro mounts.
        host("ip","netns","add",self.namespace)
        self.net_created = True
        if host("ip","route","show","10.239.254.0/30").strip():
            raise ValueError("private relay subnet already assigned")
        host("ip","link","add",self.interface,"type","veth","peer","name","peer0","netns",self.namespace)
        self.veth_created = True
        host("ip","addr","add",self.host_ip+"/30","dev",self.interface)
        host("ip","link","set",self.interface,"up")
        self.net("ip","addr","add",self.peer_ip+"/30","dev","peer0")
        self.net("ip","link","set","peer0","up")
        self.net("ip","link","set","lo","up")
        self.net("iptables","-P","OUTPUT","DROP")
        self.net("iptables","-A","OUTPUT","-o","lo","-j","ACCEPT")
        self.net("iptables","-A","OUTPUT","-d",self.host_ip,"-p","tcp","-m","multiport","--dports",",".join(str(outer) for outer,_ in RELAY_PORTS),"-j","ACCEPT")
        self.net("ip6tables","-P","OUTPUT","DROP")
        self.net("ip6tables","-A","OUTPUT","-o","lo","-j","ACCEPT")
        try:
            self.relay = FixedRelay(self.host_ip,"127.0.0.1",RELAY_PORTS)
        except BaseException as error:
            if getattr(error,"position_relay_cleanup_refused",False) is True:
                self.relay_constructor_cleanup_refused = True
            raise
        self.inner = subprocess.Popen(["ip","netns","exec",self.namespace,sys.executable,str(Path(__file__).resolve()),"--fixed-relays"],env=SAFE,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.PIPE,start_new_session=True)
        time.sleep(.5)
        if self.inner.poll() is not None: raise ValueError("private loopback relays failed startup")
        # The namespace has no default IPv4 route and no global IPv6 address.
        if self.net("ip","route","show","default").strip(): raise ValueError("unexpected namespace route")
        return self

    def net(self,*args):
        return host("ip","netns","exec",self.namespace,*args)

    def approved_mount(self, source, destination):
        candidate=Path(source)
        if candidate.is_relative_to(self.artifact_root_canonical):
            private_out=self.artifact_root_canonical/"artifacts"
            allowed={(private_out/"oms-store","/fixture/oms-store"),
                     (private_out/"position-reclaim","/fixture/position-reclaim"),
                     (private_out/"seed.json","/receipt/seed.json"),
                     (private_out/"negative-seed.json","/receipt/seed.json")}
            if (candidate,destination) not in allowed:
                raise ValueError("private artifact mount refused")

    def execute(self,args,*,seconds=540,cwd="/input/kanz",extra=None,mounts=(),output_limit=MAX_OUTPUT):
        self.capsule.verify()
        unit = "position-"+uuid.uuid4().hex
        env = effective_environment(self.environment, cwd, extra)
        private=os.fstat(self.out_fd)
        if not stat.S_ISDIR(private.st_mode) or private.st_uid!=os.geteuid() or stat.S_IMODE(private.st_mode)!=0o700:
            raise ValueError("private artifact directory mode refused")
        access_specs=[["filesystem-root","/","directory",0o755,os.X_OK],
                      ["runtime-parent","/run","directory",0o755,os.X_OK],
                      ["assigned-root",str(self.root),"directory",0o711,os.X_OK],
                      ["input-parent",str(self.inputs),"directory",0o711,os.X_OK],
                      ["public-parent",str(self.inputs/"public"),"directory",0o711,os.X_OK],
                      ["source",str(self.inputs/"source"),"directory",0o755,os.R_OK|os.X_OK],
                      ["git-metadata",str(self.inputs/"source/.git"),"directory",0o755,os.R_OK|os.X_OK],
                      ["synthetic-identity",str(self.inputs/"passwd"),"file",0o644,os.R_OK],
                      ["synthetic-identity",str(self.inputs/"group"),"file",0o644,os.R_OK],
                      ["scratch",str(self.work),"directory",0o700,os.R_OK|os.X_OK]]
        bwrap = ["bwrap","--die-with-parent","--new-session","--unshare-user","--unshare-pid","--unshare-ipc","--unshare-uts","--unshare-cgroup","--cap-drop","ALL","--uid",str(UID),"--gid",str(UID),"--proc","/proc","--dir","/dev","--dir","/dev/shm"]
        for device in ("null","zero","random","urandom"):
            bwrap += ["--dev-bind","/dev/"+device,"/dev/"+device]
        for name,target in (("fd","/proc/self/fd"),("stdin","/proc/self/fd/0"),("stdout","/proc/self/fd/1"),("stderr","/proc/self/fd/2")):
            bwrap += ["--symlink",target,"/dev/"+name]
        for name in ("/usr","/bin","/lib","/lib64"):
            if Path(name).exists():
                access_specs.append(["readonly-mount",name,"directory",None,os.R_OK|os.X_OK])
                bwrap += ["--ro-bind",name,name]
        bwrap += ["--dir","/etc","--ro-bind","/etc/hosts","/etc/hosts","--ro-bind",str(self.inputs/"passwd"),"/etc/passwd","--ro-bind",str(self.inputs/"group"),"/etc/group","--dir","/etc/ssl","--ro-bind","/etc/ssl/certs","/etc/ssl/certs","--ro-bind","/etc/fonts","/etc/fonts","--ro-bind",str(self.inputs/"source"),"/input","--bind",str(self.work),"/scratch","--dir","/tool","--dir","/deps","--dir","/fixture","--dir","/receipt"]
        access_specs += [["readonly-mount","/etc/hosts","file",None,os.R_OK],
                         ["readonly-mount","/etc/ssl/certs","directory",None,os.R_OK|os.X_OK],
                         ["readonly-mount","/etc/fonts","directory",None,os.R_OK|os.X_OK]]
        for source,dest in (*self.tools,*mounts):
            source=str(Path(source).resolve(strict=True))
            self.approved_mount(source,dest)
            if source not in self.public_mounts:
                target=self.inputs/"public"/str(len(self.public_mounts))
                if Path(source).is_dir(): target.mkdir(mode=0o755)
                else: target.touch(mode=0o644)
                host("mount","--bind",source,str(target))
                self.public_mounts[source]=target
                host("mount","-o","remount,bind,ro,nodev,nosuid",str(target))
            bwrap += ["--ro-bind",str(self.public_mounts[source]),dest]
            access_specs.append(["readonly-mount",str(self.public_mounts[source]),"directory" if Path(source).is_dir() else "file",None,os.R_OK|os.X_OK if Path(source).is_dir() else os.R_OK])
        bwrap += ["--remount-ro","/proc","--remount-ro","/","--chdir",cwd,"--clearenv"]
        for key,value in sorted(env.items()): bwrap += ["--setenv",key,value]
        bwrap += ["prlimit","--core=0","--cpu="+str(seconds),"--fsize="+str(MAX_OUTPUT),"--nofile=1024","--",*args]
        gate=self.inputs/(unit+".gate")
        outer_code='import errno,json,os,stat,sys\n'+inspect.getsource(launch_permissions)+'''
status=dict(line.split(":",1) for line in open("/proc/self/status") if ":" in line)
mapping=open("/proc/self/uid_map").read().split()
assert os.getuid()==65534 and os.getgid()==65534 and os.getgroups()==[]
assert int(status["CapEff"].strip(),16)==0 and status["NoNewPrivs"].strip()=="1"
assert mapping[:2]==["0","0"] and int(mapping[2])>65534
print("POSITION_OUTER_IDENTITY "+json.dumps({"uid":os.getuid(),"gid":os.getgid(),"groups":os.getgroups(),"cap_eff":status["CapEff"].strip(),"no_new_privileges":1,"uid_map":mapping},sort_keys=True),flush=True)
access=launch_permissions(json.loads(sys.argv[1]))
print("POSITION_LAUNCH_ACCESS "+json.dumps(access,sort_keys=True),flush=True)
if access["status"]!="passed": raise SystemExit(78)
os.execvpe(sys.argv[2],sys.argv[2:],os.environ)
'''
        command = ["systemd-run","--quiet","--wait","--pipe","--unit="+unit,"--property=MemoryMax=6G","--property=MemorySwapMax=0","--property=TasksMax=512","--property=CPUQuota=200%","--property=KillMode=control-group","--property=RuntimeMaxSec="+str(seconds),sys.executable,str(Path(__file__).resolve()),"--gated-exec",str(gate),"ip","netns","exec",self.namespace,"setpriv","--reuid=65534","--regid=65534","--clear-groups","--bounding-set=-all","--inh-caps=-all","--ambient-caps=-all","--no-new-privs","/usr/bin/python3","-I","-c",outer_code,json.dumps(access_specs),*bwrap]
        self.active.add(unit)
        process = subprocess.Popen(command,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,env=SAFE,start_new_session=True)
        data = bytearray()
        deadline = time.monotonic()+seconds+20
        group=None
        observed_host_uids=set()
        observed_processes={}
        try:
            # Inspect actual cgroup files before releasing the trusted launch
            # gate. Target source has not started and cannot access the gate.
            for _ in range(100):
                row=subprocess.run(["systemctl","show",unit,"--property=ControlGroup","--value"],env=SAFE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=3)
                group=row.stdout.decode().strip()
                if group and (Path("/sys/fs/cgroup")/group.lstrip("/")/"memory.max").exists(): break
                if process.poll() is not None: raise ValueError("cgroup launch refused before source execution")
                time.sleep(.05)
            else: raise ValueError("owned cgroup did not start")
            cg=Path("/sys/fs/cgroup")/group.lstrip("/")
            effective={k:(cg/k).read_text().strip() for k in ("memory.max","memory.swap.max","pids.max","cpu.max")}
            quota,period=map(int,effective["cpu.max"].split())
            if effective["memory.max"]!=str(6*1024**3) or effective["memory.swap.max"]!="0" or effective["pids.max"]!="512" or quota!=2*period:
                raise ValueError("effective cgroup constraints differ")
            input_fd=directory(self.inputs)
            try: new_file(input_fd,gate.name,b"release\n")
            finally: os.close(input_fd)
            with selectors.DefaultSelector() as poll:
                poll.register(process.stdout,selectors.EVENT_READ)
                while poll.get_map():
                    if time.monotonic()>deadline: raise ValueError("bounded child exceeded deadline")
                    for key,_ in poll.select(.25):
                        chunk = os.read(key.fd,65536)
                        if not chunk: poll.unregister(key.fileobj); continue
                        data.extend(chunk)
                        if len(data)>output_limit: raise ValueError("child output exceeded bound")
                    if data.startswith(b"POSITION_OUTER_IDENTITY ") and b"\n" in data and (cg/"cgroup.procs").exists():
                        for pid in (cg/"cgroup.procs").read_text().split():
                            try: status=(Path("/proc")/pid/"status").read_text()
                            except FileNotFoundError: continue
                            values=dict(line.split(":",1) for line in status.splitlines() if ":" in line)
                            uids=set(map(int,values["Uid"].split()))
                            if uids!={UID}: raise ValueError("executed target acquired a privileged host identity")
                            observed_host_uids.update(uids)
                            observed_processes[pid]={"host_pid":int(pid),"namespace_pid":int(values["NSpid"].split()[-1]),"host_uids":sorted(uids),"cap_eff":values["CapEff"].strip()}
            code = process.wait(timeout=10)
        finally:
            # Stop the entire service/cgroup before looking at any target file,
            # even if the log/timeout path failed. Never infer cleanup from flags.
            try:
                stopped=subprocess.run(["systemctl","stop",unit],env=SAFE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=30)
                if group and (Path("/sys/fs/cgroup")/group.lstrip("/")/"cgroup.procs").exists():
                    if (Path("/sys/fs/cgroup")/group.lstrip("/")/"cgroup.procs").read_text().strip(): raise ValueError("owned child processes remain after stop")
                elif stopped.returncode not in (0,5): raise ValueError("owned child stop refused")
            finally:
                if process.poll() is None: process.kill()
                process.wait(timeout=10)
                remaining=bool(group and (Path("/sys/fs/cgroup")/group.lstrip("/")/"cgroup.procs").exists() and (Path("/sys/fs/cgroup")/group.lstrip("/")/"cgroup.procs").read_text().strip())
                if not remaining: self.active.discard(unit)
                self.executions.append({"unit":unit,"command":args,"seconds":seconds,"effective":locals().get("effective"),"supervisor_exit":locals().get("code"),"combined_output_bytes":len(data),"observed_host_uids":sorted(observed_host_uids),"observed_processes":list(observed_processes.values()),"host_sampling_limit":"fast commands may terminate before parent sampling","remaining_processes":remaining})
                with contextlib.suppress(FileNotFoundError): gate.unlink()
                subprocess.run(["systemctl","reset-failed",unit],env=SAFE,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=10)
        first,separator,rest=bytes(data).partition(b"\n")
        if not separator or not first.startswith(b"POSITION_OUTER_IDENTITY "):
            self.retain("outer-identity-refusal-"+unit+".json",json.dumps(launch_observation(code,bytes(data)),sort_keys=True).encode())
            raise ValueError("trusted pre-bwrap non-root outer identity receipt absent")
        outer=json.loads(first.removeprefix(b"POSITION_OUTER_IDENTITY "))
        if outer["uid"]!=65534 or outer["gid"]!=65534 or outer["groups"] or int(outer["cap_eff"],16)!=0 or outer["no_new_privileges"]!=1: raise ValueError("outer identity differs")
        self.executions[-1]["outer_identity"]=outer
        access_frame,separator,rest=rest.partition(b"\n")
        try:
            if not separator: raise ValueError("launch permission frame absent")
            access=launch_permission_output(access_frame,[s[0] for s in access_specs])
        except ValueError:
            self.retain("launch-permissions-"+unit+".json",json.dumps({"schema":1,"classification":"missing-or-invalid-frame","supervisor_exit":code,"combined_output_bytes":len(data)},sort_keys=True).encode())
            raise ValueError("launch permission preflight framing refused") from None
        self.retain("launch-permissions-"+unit+".json",json.dumps({**access,"private_artifacts_mode700":True,"visible_inputs":self.capsule.metadata()},sort_keys=True).encode())
        self.executions[-1]["launch_permissions"]=access
        if access["status"]!="passed": raise ValueError("launch permission preflight refused")
        return code,rest

    def promote(self,relative,name,limit):
        try:
            raw = read_regular(self.work_fd,relative,limit)
        except EvidenceFileRefusal as error:
            role={"oms-store":"rust-store-executable","position-reclaim":"go-fixture-controller"}.get(name,"bounded-receipt")
            self.retain("promotion-refusal.json",json.dumps({"schema":1,"role":role,**error.observation},sort_keys=True).encode())
            raise
        self.retain(name,raw)
        return raw

    def probe(self):
        # A listening, non-allowlisted host port makes the egress refusal
        # discriminating. It is never forwarded and accepts no target traffic.
        denied=socket.socket()
        denied.bind((self.host_ip,0))
        denied.listen(1)
        denied.settimeout(.2)
        script=r'''
import errno,json,os,resource,socket,sys,time
stage="arguments"
print("POSITION_PROBE_ENTRY v1",flush=True)
try:
    expected=json.loads(sys.argv[1]); host=sys.argv[2]; denied=int(sys.argv[3])
    stage="identity"
    assert os.getuid()==65534 and os.getgid()==65534
    stage="uid-map"
    assert open("/proc/self/uid_map").read().split()[::2]==["65534","1"]
    stage="gid-map"
    assert open("/proc/self/gid_map").read().split()[::2]==["65534","1"]
    stage="environment"
    actual=set(os.environ); missing=set(expected)-actual; unexpected=actual-set(expected)
    pwd_matches=os.environ.get("PWD")==os.getcwd()
    if missing or unexpected or not pwd_matches:
        known=unexpected & {"PWD","LC_CTYPE","SHLVL","_"}
        print("POSITION_PROBE_ENVIRONMENT v1 "+json.dumps({"schema":1,"expected_count":len(expected),"observed_count":len(actual),"missing_count":len(missing),"unexpected_count":len(unexpected),"known_unexpected":sorted(known),"unknown_count":len(unexpected-known),"pwd_matches_cwd":pwd_matches},sort_keys=True),flush=True)
    assert not missing and not unexpected and pwd_matches, "ambient environment escaped"
    stage="core-limit"
    assert resource.getrlimit(resource.RLIMIT_CORE)==(0,0)
    stage="file-limit"
    assert resource.getrlimit(resource.RLIMIT_FSIZE)==(268435456,268435456)
    stage="capabilities"
    status=dict(line.split(":",1) for line in open("/proc/self/status") if ":" in line)
    assert int(status["CapEff"].strip(),16)==0
    stage="hidden-paths"
    for hidden in ("/home","/root","/var/run/docker.sock","/run/docker.sock"):
        assert not os.path.exists(hidden), "host path exposed"
    stage="input-readonly"
    for path in ("/input/AGENTS.md","/tool/go/bin/go"):
        try:
            fd=os.open(path,os.O_WRONLY); os.close(fd)
        except OSError as e:
            assert e.errno in (errno.EROFS,errno.EACCES)
        else: raise AssertionError("source/toolchain writable")
    stage="write-boundary"
    for path in ("/probe","/etc/probe","/dev/probe","/dev/shm/probe","/fixture/probe","/receipt/probe","/input/probe"):
        try:
            fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600); os.close(fd)
        except OSError as e:
            assert e.errno in (errno.EROFS,errno.EACCES)
        else: raise AssertionError("write escaped bounded scratch")
    stage="root-readonly"
    assert os.statvfs("/").f_flag & os.ST_RDONLY
    stage="device"
    with open("/dev/null","wb") as null: null.write(b"required character device")
    stage="scratch-write"
    with open("/scratch/probe","xb") as f:f.write(b"owned scratch")
    stage="private-loopback"
    with socket.socket() as listener:
        listener.bind(("127.0.0.1",0)); listener.listen(1)
        with socket.create_connection(listener.getsockname(),timeout=1) as client:
            accepted,_=listener.accept()
            with accepted: client.sendall(b"loopback"); assert accepted.recv(8)==b"loopback"
    stage="fixed-backends"
    for port in (4222,4223,4224,5432,6379,9092):
        with socket.create_connection(("127.0.0.1",port),timeout=3): pass
    stage="egress-refusal"
    for address in ((host,denied),("203.0.113.1",443)):
        try: socket.create_connection(address,timeout=.3).close()
        except OSError: pass
        else: raise AssertionError("unapproved egress succeeded")
    stage="proc-isolation"
    assert not os.path.exists("/proc/999999/status")
    stage="success-record"
    print(json.dumps({"uid":os.getuid(),"uid_map":open("/proc/self/uid_map").read().split(),"gid_map":open("/proc/self/gid_map").read().split(),"cap_eff":status["CapEff"].strip(),"network_namespace":os.stat("/proc/self/ns/net").st_ino,"pid":os.getpid(),"scratch_total":os.statvfs("/scratch").f_blocks*os.statvfs("/scratch").f_frsize,"environment_names":sorted(os.environ),"private_loopback":"passed","fixed_backends":"connected","unapproved_egress":"refused","source_and_toolchain":"readonly","cores":0},sort_keys=True))
    stage="sampling-handshake"
    sys.stdout.flush()
    time.sleep(.5) # Only a bounded kernel-observation handshake, never financial quiescence.
except AssertionError:
    print("POSITION_PROBE_REFUSAL v1 stage "+stage+" assertion",flush=True)
    raise SystemExit(1) from None
except OSError:
    print("POSITION_PROBE_REFUSAL v1 stage "+stage+" os-operation",flush=True)
    raise SystemExit(1) from None
except BaseException:
    print("POSITION_PROBE_REFUSAL v1 stage "+stage+" unexpected",flush=True)
    raise SystemExit(1) from None
'''
        try:
            expected = effective_environment(self.environment, "/input/kanz")
            code,raw=self.execute(["python3","-I","-c",script,json.dumps(sorted(expected)),self.host_ip,str(denied.getsockname()[1])],seconds=30,output_limit=65536)
            observation,raw=probe_output(code,raw,self.executions[-1]["combined_output_bytes"])
            self.retain("sandbox-probe-observation.json",json.dumps(observation,sort_keys=True).encode())
            if code: raise ValueError("kernel/mount/environment/network probes failed")
            if raw is None: raise ValueError("kernel probe diagnostic framing refused")
            try: conn,_=denied.accept()
            except socket.timeout: pass
            else:
                conn.close()
                raise ValueError("namespace target reached forbidden listening host port")
            result=json.loads(raw)
            observed=self.executions[-1]
            if observed["observed_host_uids"]!=[UID] or not any(row["namespace_pid"]==result["pid"] and row["host_uids"]==[UID] and int(row["cap_eff"],16)==0 for row in observed["observed_processes"]): raise ValueError("actual kernel-probe target host identity was not independently observed")
            result["parent_host_process_observation"]=observed["observed_processes"]
            if result["network_namespace"]==os.stat("/proc/self/ns/net").st_ino or result["scratch_total"]>5*1024**3: raise ValueError("namespace or disk limit was not isolated")
            self.retain("sandbox-probes.json",json.dumps(result,sort_keys=True).encode())
        finally: denied.close()

    def retain(self,name,raw):
        if len(raw)>MAX_OUTPUT or self.retained+len(raw)>768*1024*1024: raise ValueError("artifact exceeds byte limit")
        new_file(self.out_fd,name,raw)
        self.retained+=len(raw)
        self.artifacts[name] = {"size":len(raw),"sha256":hashlib.sha256(raw).hexdigest()}

    def close(self):
        failures = []
        if self.relay_constructor_cleanup_refused:
            failures.append("fixed_relay_constructor_cleanup_refused")
        def attempt(action):
            try: return action()
            except BaseException as error:
                failures.append(type(error).__name__)
                return None
        namespace_present=interface_present=mount_present=None
        public_remaining=[]
        relay_stopped=self.relay is None and not self.relay_constructor_cleanup_refused
        try:
            for unit in tuple(self.active):
                attempt(lambda unit=unit:host("systemctl","stop",unit))
                state=attempt(lambda unit=unit:host("systemctl","show",unit,"--property=ActiveState","--value"))
                if state is not None and state.strip() in (b"inactive",b"failed",b""): self.active.discard(unit)
            if self.inner is not None:
                if self.inner.poll() is None: attempt(self.inner.terminate)
                attempt(lambda:self.inner.wait(timeout=10))
                if self.inner.poll() is None:
                    attempt(self.inner.kill)
                    attempt(lambda:self.inner.wait(timeout=10))
            if self.relay is not None:
                relay_stopped=attempt(lambda:(self.relay.close(),True)[1]) is True
            if self.veth_created: attempt(lambda:host("ip","link","del",self.interface))
            if self.net_created: attempt(lambda:host("ip","netns","del",self.namespace))
        finally:
            # Even failed diagnostic collection must not skip descriptor close
            # or attempts to unmount every parent-owned scratch/input mount.
            if self.work_fd is not None:
                attempt(lambda:os.close(self.work_fd))
                self.work_fd=None
            for target in reversed(tuple(self.public_mounts.values())):
                attempt(lambda target=target:host("umount",str(target)))
                status=attempt(lambda target=target:subprocess.run(["mountpoint","-q",str(target)],env=SAFE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=10).returncode)
                if status is None or status==0: public_remaining.append(target.name)
            if public_remaining: failures.append("public_input_mount_remains_or_unproven")
            if self.mounted:
                result=attempt(lambda:host("umount",str(self.work)))
                if result is not None: self.mounted=False
            names=attempt(lambda:host("ip","netns","list"))
            namespace_present=None if names is None else self.namespace in names.decode().split()
            status=attempt(lambda:subprocess.run(["ip","link","show",self.interface],env=SAFE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=10).returncode)
            interface_present=None if status is None else status==0
            if self.capsule.created:
                status=attempt(lambda:subprocess.run(["mountpoint","-q",str(self.work)],env=SAFE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=10).returncode)
                mount_present=None if status is None else status==0
            else: mount_present=False # Never inspect/adopt an unallocated or colliding leaf.
            if any(value is not False for value in (namespace_present,interface_present,mount_present)) or self.active: failures.append("owned_resource_remains_or_unproven")
            retired=self.inner is None or self.inner.poll() is not None
            resources_absent=all(value is False for value in (namespace_present,interface_present,mount_present)) and not self.mounted and not self.active and not public_remaining and retired and relay_stopped and not self.relay_constructor_cleanup_refused
            attempt(lambda:self.capsule.retire(resources_absent=resources_absent))
            self.capsule_cleanup_complete=not self.capsule.created or self.capsule.absent is True
            if not self.capsule_cleanup_complete: failures.append("visible_input_capsule_remains_or_unproven")
            if self.out_fd is not None:
                try:
                    attempt(lambda:self.retain("executions.json",json.dumps(self.executions,sort_keys=True).encode()))
                    attempt(lambda:self.retain("sandbox-cleanup.json",json.dumps({"errors":failures,"relay_constructor_cleanup_refused":self.relay_constructor_cleanup_refused,"namespace":self.namespace,"namespace_present":namespace_present,"interface_present":interface_present,"mount_present":mount_present,"public_input_mounts_remaining":public_remaining,"child_units_remaining":sorted(self.active),"visible_inputs":self.capsule.metadata(),"capsule_cleanup_complete":self.capsule_cleanup_complete,"relay_stopped":relay_stopped}).encode()))
                finally:
                    os.close(self.out_fd); self.out_fd=None
        if failures: raise ValueError("owned sandbox cleanup failed: "+",".join(failures))
        self.cleanup_verified = True


if __name__ == "__main__":
    if os.geteuid()!=0:
        raise SystemExit("parent-only fixed relay invocation required")
    if len(sys.argv)>3 and sys.argv[1]=="--gated-exec":
        gate=Path(sys.argv[2])
        until=time.monotonic()+15
        while not gate.exists():
            if time.monotonic()>until: raise SystemExit("parent launch gate expired")
            time.sleep(.02)
        os.execvpe(sys.argv[3],sys.argv[3:],SAFE)
    if sys.argv[1:] != ["--fixed-relays"]:
        raise SystemExit("parent-only fixed relay invocation required")
    relay = FixedRelay("127.0.0.1","10.239.254.1",tuple((backend,outer) for outer,backend in RELAY_PORTS))
    try:
        while True: time.sleep(1)
    finally:
        relay.close()
