#!/usr/bin/env python3
"""Disposable, local-only listener proof. Never accepts an external kubeconfig."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

import yaml

# Resolved from the already used version tags on 2026-11-05; no floating pull.
IMAGES = {
    "k3s": "rancher/k3s@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657",
    "postgres": "postgres@sha256:0cefaabd882e42bcb2a2f3dc56d3ce4770560ba62fd7f197f1a2e008fd5731ab",
    "nats": "nats@sha256:991242c32d2738ffd358fd6614dd96c735017b4b3b34bfdb2c52a072d66db211",
    "nats-box": "natsio/nats-box@sha256:0784ab710aefaf6ef037ed797ee7dcde613c6ad208c4dbff1945fc7c1b5b5375",
}
TRUSTED_ROOT = Path(__file__).resolve().parents[3]
ROOT = TRUSTED_ROOT
REPO = ROOT.parent


def run(*args, data=None, timeout=120, env=None, binary=False):
    # Commands/ambient environment are deliberately not dumped: Docker's login
    # is needed by the trusted parent only, never mounted into the test cluster.
    result = subprocess.run(args, input=data, text=not binary, capture_output=True,
                            timeout=timeout, check=False, env=env)
    if result.returncode:
        raise RuntimeError(f"{args[0]} {args[1] if len(args)>1 else ''} failed ({result.returncode}): {result.stderr[-4000:]}")
    return result.stdout


def validate_inputs(image, tag, sha, service="tv-sync"):
    if service not in ("tv-sync","kanz-migrate") or not re.fullmatch(r"ghcr\.io/eighred/"+re.escape(service)+r"@sha256:[0-9a-f]{64}", image):
        raise ValueError(f"image must be an immutable eighred/{service} GHCR digest")
    if not re.fullmatch(r"v[0-9A-Za-z][0-9A-Za-z._-]{0,100}", tag):
        raise ValueError("invalid release tag")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("release SHA must be 40 lowercase hex characters")


def verify_release(image, tag, sha, service="tv-sync", candidate_sha="HEAD"):
    validate_inputs(image, tag, sha, service)
    peeled = run("git", "-C", str(REPO), "rev-parse", f"refs/tags/{tag}^{{commit}}").strip()
    if peeled != sha:
        raise RuntimeError("release tag does not resolve to expected source SHA")
    run("git", "-C", str(REPO), "merge-base", "--is-ancestor", sha, "origin/main")
    run("git", "-C", str(REPO), "merge-base", "--is-ancestor", sha, candidate_sha)
    identity = ["--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
                "--certificate-identity", f"https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/{tag}",
                "--certificate-github-workflow-sha", sha]
    run("cosign", "verify", image, *identity, timeout=180)
    for predicate in ("spdxjson", "vuln"):
        run("cosign", "verify-attestation", image, "--type", predicate, *identity, timeout=180)



def resolve_candidate(sha):
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("candidate SHA must be 40 lowercase hex characters")
    resolved = run("git", "-C", str(REPO), "rev-parse", "--verify", sha + "^{commit}").strip()
    if resolved != sha:
        raise RuntimeError("candidate does not resolve to the supplied commit")
    return sha


def export_manifests(sha, destination):
    """Read objects from trusted Git; never check out candidate executable code."""
    resolve_candidate(sha)
    listing = run("git", "-C", str(REPO), "ls-tree", "-r", "-l", "-z", sha,
                  "--", "kanz/infra", binary=True)
    sources = []
    total = 0
    for entry in listing.split(b"\0"):
        if not entry:
            continue
        header, raw_path = entry.split(b"\t", 1)
        mode, kind, oid, size = header.decode("ascii").split()
        path = raw_path.decode("utf-8")
        relative = PurePosixPath(path)
        if (relative.is_absolute() or ".." in relative.parts or "\\" in path
                or ":" in path or str(relative) != path
                or relative.parts[:2] != ("kanz", "infra")):
            raise RuntimeError("candidate manifest path escapes infra")
        if mode not in ("100644", "100755") or kind != "blob":
            raise RuntimeError("candidate infra contains a symlink or nonregular object")
        if relative.suffix.lower() not in (".yaml", ".yml", ".json"):
            continue
        length = int(size)
        total += length
        if length > 16*1024*1024 or total > 128*1024*1024:
            raise RuntimeError("candidate manifest size limit exceeded")
        if not re.fullmatch(r"[0-9a-f]{40}", oid):
            raise RuntimeError("invalid candidate blob identity")
        content = run("git", "-C", str(REPO), "show", oid, binary=True)
        if len(content) != length:
            raise RuntimeError("candidate blob size differs from tree")
        target = destination.joinpath(*relative.parts)
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("xb") as output:
            output.write(content)
        sources.append({"path": path, "blob": oid, "sha256": hashlib.sha256(content).hexdigest()})
    if not sources:
        raise RuntimeError("candidate contains no manifest blobs")
    return sources


def safe_read(path, root):
    path,root=Path(path),Path(root).resolve()
    try:
        path.resolve().relative_to(root)
        relative=path.relative_to(root)
    except ValueError:
        raise RuntimeError("candidate file escapes checkout") from None
    current=root
    for part in relative.parts:
        current=current/part
        if current.is_symlink():
            raise RuntimeError("candidate manifest path contains a symlink")
    if path.stat().st_size > 16*1024*1024:
        raise RuntimeError("candidate manifest exceeds 16 MiB")
    return path.read_text(encoding="utf-8")


def candidate_listener(deployment,svc,image,migration_image):
    for resource in (deployment,svc):
        meta=resource.get("metadata",{})
        if meta.get("name")!="tv-sync" or meta.get("namespace")!="kanz-services":
            raise RuntimeError("candidate resource identity differs from the fixture topology")
    if deployment["spec"].get("replicas") != 1 or deployment["spec"].get("strategy",{}).get("type") != "Recreate":
        raise RuntimeError("candidate must preserve singleton replicas=1 and Recreate strategy")
    spec=deployment["spec"]["template"]["spec"]
    if [x["name"] for x in spec["containers"]] != ["tv-sync"] or [x["name"] for x in spec["initContainers"]] != ["migrate"]:
        raise RuntimeError("candidate adds an unexpected executable container")
    if spec.get("hostNetwork") or spec.get("hostPID") or spec.get("hostIPC"):
        raise RuntimeError("candidate requests host namespaces")
    if any("hostPath" in v for v in spec.get("volumes",[])):
        raise RuntimeError("candidate requests host filesystem access")
    for container in spec["containers"]+spec["initContainers"]:
        if container.get("securityContext",{}).get("privileged"):
            raise RuntimeError("candidate requests privileged workload")
    migrate=next(x for x in spec["initContainers"] if x["name"]=="migrate")
    if migrate.get("image") != migration_image:
        raise RuntimeError("candidate migration image differs from supplied released digest")
    app=next(x for x in spec["containers"] if x["name"]=="tv-sync")
    if any(p.get("hostPort") or p.get("hostIP") for p in app.get("ports",[])):
        raise RuntimeError("candidate requests host port binding")
    if {p.get("containerPort") for p in app.get("ports",[])} != {8091,8113}:
        raise RuntimeError("candidate must declare only the two listener ports")
    declared={x["name"]:x.get("value") for x in app["env"]}
    if declared.get("TV_SYNC_API_LISTEN")!=":8113" or declared.get("TV_SYNC_LISTEN")!=":8091":
        raise RuntimeError("candidate manifests have not opted into split listeners")
    if app["image"]!=image:
        raise RuntimeError("candidate deployment image differs from supplied released digest")
    if not any(p["port"]==8113 and p["targetPort"]==8113 for p in svc["spec"]["ports"]):
        raise RuntimeError("candidate Service does not publish API8113")
    return spec,app,declared


def policy_documents(documents):
    for document in documents:
        if document.get("kind") in ("List","NetworkPolicyList"):
            items=document.get("items")
            if not isinstance(items,list) or any(not isinstance(x,dict) for x in items):
                raise RuntimeError("malformed Kubernetes resource list")
            yield from policy_documents(items)
        else:
            yield document


def policy_inventory(root):
    policies=[]
    sources=[]
    for path in sorted((root/"infra").rglob("*")):
        if path.is_symlink():
            raise RuntimeError("candidate infra contains a symlink")
        if not path.is_file() or path.suffix.lower() not in (".yaml",".yml",".json"):
            continue
        raw=safe_read(path,root)
        try:
            documents=[d for d in yaml.safe_load_all(raw) if isinstance(d, dict)]
        except yaml.YAMLError:
            if "NetworkPolicy" in raw:
                raise RuntimeError(f"unparseable candidate policy source: {path.relative_to(root)}")
            continue
        for document in policy_documents(documents):
            if document.get("kind") != "NetworkPolicy":
                continue
            policy=json.loads(json.dumps(document))
            source=str(path.relative_to(root))
            sources.append({"source":source,"name":policy["metadata"]["name"],"namespace":policy["metadata"].get("namespace"),"spec_sha256":hashlib.sha256(json.dumps(policy["spec"],sort_keys=True).encode()).hexdigest()})
            if not policy["metadata"].get("namespace"):
                raise RuntimeError(f"policy source has no resolved namespace: {source}")
            # Overlay/base policies sharing names must not overwrite each
            # other. Their union is conservative for ingress exposure.
            policy["metadata"]={"name":f"isolation-policy-{len(policies)}","namespace":policy["metadata"]["namespace"]}
            policies.append(policy)
    if len(policies)<20:
        raise RuntimeError("policy inventory unexpectedly empty or truncated")
    return policies, sources


def wait_until(check, description, seconds=180):
    deadline = time.monotonic() + seconds
    last = "not ready"
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (RuntimeError, subprocess.TimeoutExpired) as exc:
            last = str(exc)
        time.sleep(2)
    raise RuntimeError(f"timeout waiting for {description}: {last}")


class Cluster:
    def __init__(self, name):
        self.name = name

    def k(self, *args, data=None, timeout=120):
        return run("docker", "exec", "-i", self.name, "k3s", "kubectl", *args, data=data, timeout=timeout)

    def apply(self, docs):
        self.k("apply", "-f", "-", data=json.dumps({"apiVersion":"v1", "kind":"List", "items":docs}))

    def read_yaml(self, path):
        root=TRUSTED_ROOT if path.is_relative_to(TRUSTED_ROOT) else ROOT
        return [d for d in yaml.safe_load_all(safe_read(path,root)) if isinstance(d, dict)]

    def wait_pod(self, ns, label):
        self.k("-n", ns, "wait", "--for=condition=Ready", "pod", "-l", label, "--timeout=180s", timeout=190)

    def load(self, image, local_name, scratch):
        run("docker", "pull", "--platform", "linux/amd64", image, timeout=240)
        original_id = run("docker", "image", "inspect", image, "--format", "{{.Id}}").strip()
        run("docker", "tag", image, local_name)
        archive = scratch / "image.tar"
        run("docker", "save", "-o", str(archive), local_name, timeout=180)
        run("docker", "cp", str(archive), f"{self.name}:/tmp/image.tar", timeout=180)
        run("docker", "exec", self.name, "k3s", "ctr", "images", "import", "/tmp/image.tar", timeout=180)
        imported = json.loads(run("docker", "exec", self.name, "k3s", "crictl", "inspecti", local_name))
        if imported["status"]["id"] != original_id:
            raise RuntimeError("imported image config differs from digest-pulled image")
        archive.unlink()
        return original_id


def pod(name, ns, image, command=None, env=None, app=None):
    container = {"name":name, "image":image, "imagePullPolicy":"Never"}
    if command:
        container["command"]=command
    if env:
        container["env"]=[{"name":key,"value":value} for key,value in env.items()]
    return {"apiVersion":"v1","kind":"Pod","metadata":{"name":name,"namespace":ns,"labels":{"app":app or name}},
            "spec":{"automountServiceAccountToken":False,"restartPolicy":"Never","containers":[container]}}


def service(name, ns, port):
    return {"apiVersion":"v1","kind":"Service","metadata":{"name":name,"namespace":ns},
            "spec":{"selector":{"app":name},"ports":[{"port":port,"targetPort":port}]}}


def exercise(cluster, targets, phase, evidence):
    sources=[("kanz-services","gateway"),("kanz-observability","monitor"),
             ("kanz-services","unrelated"),("isolation-other","outsider")]
    for ns, name in sources:
        for target in targets:
            value=json.loads(cluster.k("-n",ns,"exec",name,"--","/probe",f"http://{target}:8113/broker/accounts","fixture-tenant", timeout=10))
            allowed = phase=="baseline" or name=="gateway"
            if allowed:
                if value["status"] != 200 or value["error"] or "accounts" not in json.loads(value["body"]):
                    raise RuntimeError(f"{phase}: {ns}/{name} cannot read accounts through {target}")
            elif value["status"] != 0 or not value["error"]:
                raise RuntimeError(f"{phase}: forbidden API reachable from {ns}/{name} through {target}")
            evidence.append({"phase":phase,"source":ns+"/"+name,"target":target,"port":8113,"allowed":allowed,"observed_status":value["status"]})
    for target in targets:
        for path, status in [("/metrics",200),("/broker/accounts",404),("/healthz",200),("/readyz",200)]:
            value=json.loads(cluster.k("-n","kanz-observability","exec","monitor","--","/probe",f"http://{target}:8091{path}","fixture-tenant", timeout=10))
            if value["status"] != status or value["error"]:
                raise RuntimeError(f"{phase}: operations {path} returned {value['status']}, expected {status}")
            evidence.append({"phase":phase,"source":"monitor","target":target,"port":8091,"path":path,"observed_status":status})
        value=json.loads(cluster.k("-n","kanz-services","exec","gateway","--","/probe",f"http://{target}:8113/broker/accounts","", timeout=10))
        if value["status"] != 401:
            raise RuntimeError("API accepted a missing principal")


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate-sha",required=True)
    parser.add_argument("--image",required=True)
    parser.add_argument("--migration-image",required=True)
    parser.add_argument("--release-tag",required=True)
    parser.add_argument("--release-sha",required=True)
    parser.add_argument("--evidence",required=True,type=Path)
    args=parser.parse_args()
    global ROOT
    candidate_sha=resolve_candidate(args.candidate_sha)
    if sys.platform != "linux":
        raise RuntimeError("requires a disposable Linux Docker host")
    verify_release(args.image,args.release_tag,args.release_sha,candidate_sha=candidate_sha)
    verify_release(args.migration_image,args.release_tag,args.release_sha,"kanz-migrate",candidate_sha=candidate_sha)
    name="kanz-tv-isolation-"+uuid.uuid4().hex[:12]
    c=Cluster(name)
    evidence={"candidate_sha":candidate_sha,
              "harness_sha":run("git","-C",str(TRUSTED_ROOT.parent),"rev-parse","HEAD").strip(),
              "release_sha":args.release_sha,"release_tag":args.release_tag,"released_image":args.image,"released_migration_image":args.migration_image,
              "dependencies":IMAGES,"observations":[],"result":"failed"}
    with tempfile.TemporaryDirectory(prefix="kanz-tv-isolation-") as temp:
        scratch=Path(temp)
        evidence["manifest_sources"]=export_manifests(candidate_sha,scratch/"candidate")
        ROOT=scratch/"candidate/kanz"
        try:
            run("docker","run","-d","--name",name,"--label","io.kanz.tv-sync-isolation=true","--privileged",f"--cpus={min(4, os.cpu_count() or 1)}","--memory=4g",
                "--pids-limit=4096","--tmpfs","/run","--tmpfs","/var/run",IMAGES["k3s"],
                "server","--disable=traefik","--disable=servicelb","--disable=metrics-server",timeout=240)
            wait_until(lambda: json.loads(c.k("get","nodes","-o","json",timeout=15))["items"],"K3s nodes")
            c.k("wait","--for=condition=Ready","node","--all","--timeout=180s",timeout=190)
            docs=c.read_yaml(ROOT/"infra/deploy/tv-sync-deploy.yaml")
            deployment=next(d for d in docs if d["kind"]=="Deployment")
            svc=next(d for d in docs if d["kind"]=="Service")
            spec,app,declared=candidate_listener(deployment,svc,args.image,args.migration_image)
            policies,sources=policy_inventory(ROOT)
            evidence["policy_sources"]=sources
            evidence["policy_scope"]="conservative union of candidate infra policies; mutually exclusive overlays may be combined"
            evidence["policy_sha256"]=hashlib.sha256(json.dumps(policies,sort_keys=True).encode()).hexdigest()
            namespaces={"kanz-services","kanz-messaging","kanz-observability","isolation-other"}
            namespaces.update(p["metadata"]["namespace"] for p in policies)
            c.apply([{"apiVersion":"v1","kind":"Namespace","metadata":{"name":ns}} for ns in sorted(namespaces)])
            local={key:"docker.io/kanz-isolation/"+key+":"+name for key in ["postgres","nats","nats-box","tv-sync","migrate"]}
            images={**{k:v for k,v in IMAGES.items() if k!="k3s"},"tv-sync":args.image,"migrate":args.migration_image}
            evidence["imported_config_ids"]={key:c.load(image,local[key],scratch) for key,image in images.items()}
            # This probe is repository-owned stdlib code, not a downloaded tool.
            probe=scratch/"probe"
            env={**os.environ,"GOOS":"linux","GOARCH":"amd64","CGO_ENABLED":"0"}
            run("go","build","-o",str(probe),str(Path(__file__).parent/"probe/main.go"),env=env,timeout=180)
            shutil.copyfile(Path(__file__).parent/"probe/Dockerfile",scratch/"Dockerfile")
            probe_image="docker.io/kanz-isolation/probe:"+name
            run("docker","build","-t",probe_image,str(scratch),timeout=120)
            # Already local: importing uses the same image-ID check without a pull.
            run("docker","save","-o",str(scratch/"probe.tar"),probe_image,timeout=120)
            run("docker","cp",str(scratch/"probe.tar"),name+":/tmp/probe.tar")
            run("docker","exec",name,"k3s","ctr","images","import","/tmp/probe.tar")
            c.apply([{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"nats-config","namespace":"kanz-messaging"},"data":{"nats-dev.conf":(TRUSTED_ROOT/"test/backing/nats-dev.conf").read_text(encoding="utf-8")}}])
            nats=pod("nats","kanz-messaging",local["nats"],command=["nats-server","-c","/config/nats-dev.conf"])
            nats["spec"]["containers"][0]["readinessProbe"]={"tcpSocket":{"port":4222},"periodSeconds":2}
            nats["spec"]["volumes"]=[{"name":"config","configMap":{"name":"nats-config"}}]
            nats["spec"]["containers"][0]["volumeMounts"]=[{"name":"config","mountPath":"/config","readOnly":True}]
            c.apply([pod("postgres","kanz-services",local["postgres"],env={"POSTGRES_PASSWORD":"fixture-only","POSTGRES_DB":"postgres"}),service("postgres","kanz-services",5432),
                     nats,service("nats","kanz-messaging",4222)])
            wait_until(lambda: "accepting connections" in c.k("-n","kanz-services","exec","postgres","--","pg_isready","-U","postgres",timeout=15),"Postgres")
            c.k("-n","kanz-services","exec","-i","postgres","--","psql","-U","postgres","-v","ON_ERROR_STOP=1",data="CREATE ROLE tvfixture LOGIN PASSWORD 'fixture-only' NOSUPERUSER NOBYPASSRLS;\nCREATE DATABASE tvfixture OWNER tvfixture;\n")
            flags=c.k("-n","kanz-services","exec","postgres","--","psql","-U","postgres","-Atqc","SELECT rolsuper::text || ',' || rolbypassrls::text FROM pg_roles WHERE rolname='tvfixture'").strip()
            if flags!="false,false": raise RuntimeError("fixture role bypasses RLS")
            evidence["database_role"]={"superuser":False,"bypassrls":False}
            bootstrap=next(d for d in c.read_yaml(TRUSTED_ROOT/"infra/nats/bootstrap-job.yaml") if d["kind"]=="ConfigMap" and d["metadata"]["name"]=="nats-bootstrap")["data"]["bootstrap.sh"]
            box=pod("nats-bootstrap","kanz-messaging",local["nats-box"],command=["sh","-c",bootstrap],env={"NATS_URL":"nats://nats:4222","NATS_REPLICAS":"1"})
            c.wait_pod("kanz-messaging","app=nats")
            c.apply([box])
            wait_until(lambda:json.loads(c.k("-n","kanz-messaging","get","pod","nats-bootstrap","-o","json"))["status"].get("phase")=="Succeeded","stream bootstrap")
            # Only listener/probe/label wiring is read from the candidate.
            # Executable images and fixture infrastructure are independently fixed.
            replacements={"TV_SYNC_LISTEN":declared["TV_SYNC_LISTEN"],"TV_SYNC_API_LISTEN":declared["TV_SYNC_API_LISTEN"],"TV_SYNC_NATS_URL":"nats://nats.kanz-messaging.svc:4222","TV_SYNC_DATABASE_URL":"postgres://tvfixture:fixture-only@postgres:5432/tvfixture?sslmode=disable","TV_SYNC_TENANT":"fixture-tenant","TV_SYNC_SOURCE":"tv-sync/isolation"}
            security={"allowPrivilegeEscalation":False,"readOnlyRootFilesystem":True,"runAsNonRoot":True,"runAsUser":65532,"capabilities":{"drop":["ALL"]}}
            safe_app={"name":"tv-sync","image":local["tv-sync"],"imagePullPolicy":"Never","ports":[{k:v for k,v in p.items() if k in ("name","containerPort","protocol")} for p in app["ports"]],"env":[{"name":k,"value":v} for k,v in replacements.items()],"securityContext":security,"volumeMounts":[{"name":"tmp","mountPath":"/tmp"}]}
            for key in ("startupProbe","livenessProbe","readinessProbe"):
                probe=app.get(key)
                if not probe or "httpGet" not in probe or any(x in probe for x in ("exec","tcpSocket","grpc")):
                    raise RuntimeError("candidate probe is not an HTTP operations probe")
                http=probe["httpGet"]
                if http.get("port")!=8091 or http.get("path") not in ("/healthz","/readyz") or http.get("host"):
                    raise RuntimeError("candidate probe is not bound to operations8091")
                safe_app[key]={k:v for k,v in probe.items() if k in ("periodSeconds","timeoutSeconds","failureThreshold","initialDelaySeconds","successThreshold")}
                safe_app[key]["httpGet"]={"path":http["path"],"port":http["port"]}
            safe_migrate={"name":"migrate","image":local["migrate"],"imagePullPolicy":"Never","args":["--dir","/migrations/tv-sync"],"env":[{"name":"KANZ_MIGRATE_DATABASE_URL","value":replacements["TV_SYNC_DATABASE_URL"]}],"securityContext":security}
            candidate_labels=deployment["spec"]["template"]["metadata"]["labels"]
            if candidate_labels.get("app")!="tv-sync" or svc["spec"]["selector"].get("app")!="tv-sync":
                raise RuntimeError("candidate labels no longer select tv-sync")
            deployment={"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"tv-sync","namespace":"kanz-services"},"spec":{"replicas":1,"strategy":{"type":"Recreate"},"selector":{"matchLabels":{"app":"tv-sync"}},"template":{"metadata":{"labels":candidate_labels},"spec":{"automountServiceAccountToken":False,"securityContext":{"runAsNonRoot":True,"runAsUser":65532,"runAsGroup":65532,"fsGroup":65532,"seccompProfile":{"type":"RuntimeDefault"}},"volumes":[{"name":"tmp","emptyDir":{}}],"containers":[safe_app],"initContainers":[safe_migrate]}}}}
            svc={"apiVersion":"v1","kind":"Service","metadata":{"name":"tv-sync","namespace":"kanz-services"},"spec":{"type":"ClusterIP","selector":svc["spec"]["selector"],"ports":svc["spec"]["ports"]}}
            c.apply([svc,deployment])
            c.k("-n","kanz-services","rollout","status","deployment/tv-sync","--timeout=180s",timeout=190)
            sources=[("gateway","kanz-services","api-gateway"),("monitor","kanz-observability","monitor"),("unrelated","kanz-services","isolation-probe"),("outsider","isolation-other","outsider")]
            c.apply([pod(name,ns,probe_image,command=["/probe","wait"],app=app_label) for name,ns,app_label in sources])
            for source_name,ns,app_label in sources: c.wait_pod(ns,"app="+app_label)
            target=json.loads(c.k("-n","kanz-services","get","pods","-l","app=tv-sync","-o","json"))["items"][0]
            targets=[target["status"]["podIP"],json.loads(c.k("-n","kanz-services","get","service","tv-sync","-o","json"))["spec"]["clusterIP"]]
            evidence["running_image_ids"]={}
            for role,status_key,container_name in (("tv-sync","containerStatuses","tv-sync"),("migrate","initContainerStatuses","migrate")):
                status=next(x for x in target["status"][status_key] if x["name"]==container_name)
                image_id=status["imageID"]
                evidence["running_image_ids"][role]=image_id
                running=json.loads(run("docker","exec",c.name,"k3s","crictl","inspecti",image_id))
                if running["status"]["id"] != evidence["imported_config_ids"][role]:
                    raise RuntimeError(f"running {role} is not the verified imported release image")
                if role=="migrate" and status.get("state",{}).get("terminated",{}).get("exitCode")!=0:
                    raise RuntimeError("verified migration image did not complete successfully")
            exercise(c,targets,"baseline",evidence["observations"])
            c.apply(policies)
            # Permit only the unrelated in-namespace probe's outbound requests.
            # Otherwise an egress denial could impersonate API ingress isolation.
            c.apply([{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"fixture-probe-egress","namespace":"kanz-services"},"spec":{"podSelector":{"matchLabels":{"app":"isolation-probe"}},"policyTypes":["Egress"],"egress":[{"to":[{"podSelector":{"matchLabels":{"app":"tv-sync"}}}],"ports":[{"protocol":"TCP","port":8113},{"protocol":"TCP","port":8091}]}]}}])
            installed=json.loads(c.k("get","networkpolicy","-A","-o","json"))
            evidence["installed_policy_sha256"]=hashlib.sha256(json.dumps(installed,sort_keys=True).encode()).hexdigest()
            wait_until(lambda: enforced(c,targets),"policy convergence",90)
            exercise(c,targets,"enforced",evidence["observations"])
            evidence["result"]="passed"
        finally:
            # UUID-addressed cleanup never touches a preexisting user cluster.
            cleanup=subprocess.run(["docker","rm","-f","-v",c.name],capture_output=True,timeout=45,check=False)
            evidence["cleanup"]="passed" if cleanup.returncode==0 else "failed"
            if cleanup.returncode:
                evidence["result"]="failed"
            args.evidence.parent.mkdir(parents=True,exist_ok=True)
            args.evidence.write_text(json.dumps(evidence,indent=2)+"\n",encoding="utf-8")
            if cleanup.returncode:
                raise RuntimeError("disposable cluster cleanup failed")



def enforced(cluster,targets):
    trial=[]
    try:
        exercise(cluster,targets,"enforced",trial)
        return True
    except RuntimeError:
        return False


if __name__=="__main__":
    main()
