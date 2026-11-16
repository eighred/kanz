"""Narrow Linux execution boundary for the disposable #1014 CI proof.

The parent owns Docker, namespace setup and artifact promotion. Target processes
get a read-only source copy, public tools/dependencies, private loopback and only
the fixed CI backend relays below. There is no generic forwarding request format.

Execution requires prior review of the exact checked head of kanz-ci.yml and
both privileged parent programs, position_reclaim_proof.py and this module.
Child restrictions cannot contain a modified workflow or either parent program.
"""
import contextlib
import hashlib
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
SAFE = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8", "GIT_CONFIG_NOSYSTEM":"1", "GIT_CONFIG_GLOBAL":"/dev/null"}


def host(*args, timeout=30, input=None):
    process=subprocess.Popen(args,stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,env=SAFE,start_new_session=True)
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
                raise ValueError("linked, nonregular or oversized evidence")
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
    def __init__(self, repo, root, tools, env):
        if sys.platform != "linux" or os.geteuid()!=0:
            raise ValueError("Linux root parent orchestration required")
        self.repo, self.root, self.tools = Path(repo), Path(root), tools
        self.environment = env
        self.suffix = uuid.uuid4().hex[:10]
        self.namespace = "position-"+self.suffix
        self.interface = "pr"+self.suffix
        # No route leaves this /30. Firewall also refuses host destinations
        # other than the seven fixed transit ports (24225 reaches the negative broker).
        self.host_ip,self.peer_ip = "10.239.254.1","10.239.254.2"
        self.work = self.root/"scratch"
        self.inputs = self.root/"inputs"
        self.out = self.root/"artifacts"
        self.active = set()
        self.relay = None
        self.relay_constructor_cleanup_refused = False
        self.inner = None
        self.mounted = self.net_created = self.veth_created = False
        self.work_fd = self.out_fd = self.receipts_fd = None
        self.retained = 0
        self.artifacts = {}
        self.cleanup_verified = False
        self.executions = []
        self.public_mounts = {}

    def start(self):
        for command in ("bwrap","setpriv","systemd-run","ip","iptables","ip6tables","mount","umount","prlimit"):
            if not shutil.which(command,path=SAFE["PATH"]): raise ValueError("missing enforced isolation tool: "+command)
        for path in (self.work,self.inputs,self.out): path.mkdir(mode=0o700)
        self.root.chmod(0o711)
        self.inputs.chmod(0o711)
        (self.inputs/"public").mkdir(mode=0o711)
        self.out_fd = directory(self.out)
        self.receipts_fd = os.dup(self.out_fd)
        host("mount","-t","tmpfs","-o","size=5G,nodev,nosuid,mode=0700,uid=65534,gid=65534","position-proof",str(self.work))
        self.mounted = True
        self.work_fd = directory(self.work)
        for name in ("tmp","home","cache","cargo","rust-target"):
            (self.work/name).mkdir(mode=0o700)
            os.chown(self.work/name,UID,UID)
        # Source copy is parent-owned and never writable by target processes.
        source = self.inputs/"source"
        source.mkdir(mode=0o755)
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
                dest = source/name
                dest.parent.mkdir(parents=True,exist_ok=True)
                dest.write_bytes(data)
                # The tracked executable bit matters to fixtures and scripts.
                dest.chmod(0o755 if (self.repo/name).stat().st_mode & 0o111 else 0o644)
        finally:
            os.close(repo_fd)
        for name in ("kanz-schemas/gen/go","kanz-web/node_modules","kanz-web/dist"):
            (source/name).mkdir(parents=True,exist_ok=True)
        # A private exact-head Git object/index copy preserves VCS and index
        # tests without exposing the checkout's config, auth header or worktree.
        source_sha=host("git","-c","safe.directory="+str(self.repo),"-C",str(self.repo),"rev-parse","HEAD").decode().strip()
        host("git","init","--template=","--quiet",str(source))
        host("git","-c","safe.directory="+str(self.repo),"-c","core.hooksPath=/dev/null","-C",str(source),"fetch","--quiet","--no-tags","--depth=1",str(self.repo),source_sha,timeout=90)
        host("git","-C",str(source),"reset","--quiet","--mixed",source_sha)
        (source/".git/FETCH_HEAD").unlink()
        # A real sysroot is read-only; these synthetic identity files contain no
        # host accounts and make user lookup deterministic inside the boundary.
        (self.inputs/"passwd").write_text("nobody:x:65534:65534:fixture:/scratch/home:/usr/sbin/nologin\n")
        (self.inputs/"group").write_text("nogroup:x:65534:\n")
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

    def execute(self,args,*,seconds=540,cwd="/input/kanz",extra=None,mounts=(),output_limit=MAX_OUTPUT):
        unit = "position-"+uuid.uuid4().hex
        env = dict(self.environment)
        if extra: env.update(extra)
        bwrap = ["bwrap","--die-with-parent","--new-session","--unshare-user","--unshare-pid","--unshare-ipc","--unshare-uts","--unshare-cgroup","--cap-drop","ALL","--uid",str(UID),"--gid",str(UID),"--proc","/proc","--dir","/dev","--dir","/dev/shm"]
        for device in ("null","zero","random","urandom"):
            bwrap += ["--dev-bind","/dev/"+device,"/dev/"+device]
        for name,target in (("fd","/proc/self/fd"),("stdin","/proc/self/fd/0"),("stdout","/proc/self/fd/1"),("stderr","/proc/self/fd/2")):
            bwrap += ["--symlink",target,"/dev/"+name]
        for name in ("/usr","/bin","/lib","/lib64"):
            if Path(name).exists(): bwrap += ["--ro-bind",name,name]
        bwrap += ["--dir","/etc","--ro-bind","/etc/hosts","/etc/hosts","--ro-bind",str(self.inputs/"passwd"),"/etc/passwd","--ro-bind",str(self.inputs/"group"),"/etc/group","--dir","/etc/ssl","--ro-bind","/etc/ssl/certs","/etc/ssl/certs","--ro-bind","/etc/fonts","/etc/fonts","--ro-bind",str(self.inputs/"source"),"/input","--bind",str(self.work),"/scratch","--dir","/tool","--dir","/deps","--dir","/fixture","--dir","/receipt"]
        for source,dest in (*self.tools,*mounts):
            source=str(Path(source).resolve(strict=True))
            if source not in self.public_mounts:
                target=self.inputs/"public"/str(len(self.public_mounts))
                if Path(source).is_dir(): target.mkdir(mode=0o755)
                else: target.touch(mode=0o644)
                host("mount","--bind",source,str(target))
                self.public_mounts[source]=target
                host("mount","-o","remount,bind,ro,nodev,nosuid",str(target))
            bwrap += ["--ro-bind",str(self.public_mounts[source]),dest]
        bwrap += ["--remount-ro","/proc","--remount-ro","/","--chdir",cwd,"--clearenv"]
        for key,value in sorted(env.items()): bwrap += ["--setenv",key,value]
        bwrap += ["prlimit","--core=0","--cpu="+str(seconds),"--fsize="+str(MAX_OUTPUT),"--nofile=1024","--",*args]
        gate=self.inputs/(unit+".gate")
        outer_code='''import json,os,sys
status=dict(line.split(":",1) for line in open("/proc/self/status") if ":" in line)
mapping=open("/proc/self/uid_map").read().split()
assert os.getuid()==65534 and os.getgid()==65534 and os.getgroups()==[]
assert int(status["CapEff"].strip(),16)==0 and status["NoNewPrivs"].strip()=="1"
assert mapping[:2]==["0","0"] and int(mapping[2])>65534
print("POSITION_OUTER_IDENTITY "+json.dumps({"uid":os.getuid(),"gid":os.getgid(),"groups":os.getgroups(),"cap_eff":status["CapEff"].strip(),"no_new_privileges":1,"uid_map":mapping},sort_keys=True),flush=True)
os.execvpe(sys.argv[1],sys.argv[1:],os.environ)
'''
        command = ["systemd-run","--quiet","--wait","--pipe","--unit="+unit,"--property=MemoryMax=6G","--property=MemorySwapMax=0","--property=TasksMax=512","--property=CPUQuota=200%","--property=KillMode=control-group","--property=RuntimeMaxSec="+str(seconds),sys.executable,str(Path(__file__).resolve()),"--gated-exec",str(gate),"ip","netns","exec",self.namespace,"setpriv","--reuid=65534","--regid=65534","--clear-groups","--bounding-set=-all","--inh-caps=-all","--ambient-caps=-all","--no-new-privs","/usr/bin/python3","-I","-c",outer_code,*bwrap]
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
                self.executions.append({"unit":unit,"command":args,"seconds":seconds,"effective":locals().get("effective"),"observed_host_uids":sorted(observed_host_uids),"observed_processes":list(observed_processes.values()),"host_sampling_limit":"fast commands may terminate before parent sampling","remaining_processes":remaining})
                with contextlib.suppress(FileNotFoundError): gate.unlink()
                subprocess.run(["systemctl","reset-failed",unit],env=SAFE,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=10)
        first,separator,rest=bytes(data).partition(b"\n")
        if not separator or not first.startswith(b"POSITION_OUTER_IDENTITY "):
            self.retain("outer-identity-refusal-"+unit+".log",bytes(data))
            raise ValueError("trusted pre-bwrap non-root outer identity receipt absent")
        outer=json.loads(first.removeprefix(b"POSITION_OUTER_IDENTITY "))
        if outer["uid"]!=65534 or outer["gid"]!=65534 or outer["groups"] or int(outer["cap_eff"],16)!=0 or outer["no_new_privileges"]!=1: raise ValueError("outer identity differs")
        self.executions[-1]["outer_identity"]=outer
        return code,rest

    def promote(self,relative,name,limit):
        raw = read_regular(self.work_fd,relative,limit)
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
expected=json.loads(sys.argv[1]); host=sys.argv[2]; denied=int(sys.argv[3])
assert os.getuid()==65534 and os.getgid()==65534
assert open("/proc/self/uid_map").read().split()[::2]==["65534","1"]
assert open("/proc/self/gid_map").read().split()[::2]==["65534","1"]
assert set(os.environ)==set(expected), "ambient environment escaped"
assert resource.getrlimit(resource.RLIMIT_CORE)==(0,0)
assert resource.getrlimit(resource.RLIMIT_FSIZE)==(268435456,268435456)
status=dict(line.split(":",1) for line in open("/proc/self/status") if ":" in line)
assert int(status["CapEff"].strip(),16)==0
for hidden in ("/home","/root","/var/run/docker.sock","/run/docker.sock"):
    assert not os.path.exists(hidden), "host path exposed"
for path in ("/input/AGENTS.md","/tool/go/bin/go"):
    try:
        fd=os.open(path,os.O_WRONLY); os.close(fd)
    except OSError as e:
        assert e.errno in (errno.EROFS,errno.EACCES)
    else: raise AssertionError("source/toolchain writable")
for path in ("/probe","/etc/probe","/dev/probe","/dev/shm/probe","/fixture/probe","/receipt/probe","/input/probe"):
    try:
        fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600); os.close(fd)
    except OSError as e:
        assert e.errno in (errno.EROFS,errno.EACCES)
    else: raise AssertionError("write escaped bounded scratch")
assert os.statvfs("/").f_flag & os.ST_RDONLY
with open("/dev/null","wb") as null: null.write(b"required character device")
with open("/scratch/probe","xb") as f:f.write(b"owned scratch")
with socket.socket() as listener:
    listener.bind(("127.0.0.1",0)); listener.listen(1)
    with socket.create_connection(listener.getsockname(),timeout=1) as client:
        accepted,_=listener.accept()
        with accepted: client.sendall(b"loopback"); assert accepted.recv(8)==b"loopback"
for port in (4222,4223,4224,5432,6379,9092):
    with socket.create_connection(("127.0.0.1",port),timeout=3): pass
for address in ((host,denied),("203.0.113.1",443)):
    try: socket.create_connection(address,timeout=.3).close()
    except OSError: pass
    else: raise AssertionError("unapproved egress succeeded")
assert not os.path.exists("/proc/999999/status")
print(json.dumps({"uid":os.getuid(),"uid_map":open("/proc/self/uid_map").read().split(),"gid_map":open("/proc/self/gid_map").read().split(),"cap_eff":status["CapEff"].strip(),"network_namespace":os.stat("/proc/self/ns/net").st_ino,"pid":os.getpid(),"scratch_total":os.statvfs("/scratch").f_blocks*os.statvfs("/scratch").f_frsize,"environment_names":sorted(os.environ),"private_loopback":"passed","fixed_backends":"connected","unapproved_egress":"refused","source_and_toolchain":"readonly","cores":0},sort_keys=True))
sys.stdout.flush()
time.sleep(.5) # Only a bounded kernel-observation handshake, never financial quiescence.
'''
        try:
            code,raw=self.execute(["python3","-I","-c",script,json.dumps(sorted(self.environment)),self.host_ip,str(denied.getsockname()[1])],seconds=30,output_limit=65536)
            if code: raise ValueError("kernel/mount/environment/network probes failed")
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
            if self.relay is not None: attempt(self.relay.close)
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
            status=attempt(lambda:subprocess.run(["mountpoint","-q",str(self.work)],env=SAFE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=10).returncode)
            mount_present=None if status is None else status==0
            if any(value is not False for value in (namespace_present,interface_present,mount_present)) or self.active: failures.append("owned_resource_remains_or_unproven")
            if self.out_fd is not None:
                try:
                    attempt(lambda:self.retain("executions.json",json.dumps(self.executions,sort_keys=True).encode()))
                    attempt(lambda:self.retain("sandbox-cleanup.json",json.dumps({"errors":failures,"relay_constructor_cleanup_refused":self.relay_constructor_cleanup_refused,"namespace":self.namespace,"namespace_present":namespace_present,"interface_present":interface_present,"mount_present":mount_present,"public_input_mounts_remaining":public_remaining,"child_units_remaining":sorted(self.active)}).encode()))
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
