"""Verify the committed Grafana deployment on an owned disposable Cilium cluster.

No existing kubeconfig, credentials, cluster or service is accepted. Public
observations only are retained; the independently generated dummy admin password
enters Kubernetes on stdin and is never written to an artifact or command line.
"""
import base64
import contextlib
import copy
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import re
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

import yaml

ROOT = Path(__file__).resolve().parents[2]
OBS = ROOT / "kanz/infra/observability"
NS = "kanz-observability"
MISSING_NS = "grafana-missing-secret"
BUSYBOX = "busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
PYTHON = "python:3.13.18-alpine3.24@sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c"
INPUTS = ("grafana.yaml", "prometheus.yaml", "rules-configmap.yaml", "dashboards/slo-burn-rate.json")
CASES = (
    "loaded_enforcing_cilium", "missing_admin_secret_refuses_startup",
    "committed_grafana_ready", "committed_prometheus_ready", "exact_public_mounts_and_permissions",
    "unauthenticated_dashboard_refused", "provisioned_datasource_and_dashboard",
    "nonempty_panel_queries_match_prometheus_and_oracle", "grafana_to_prometheus_allowed",
    "grafana_to_other_9090_denied", "untrusted_to_grafana_denied",
    "removed_policy_allows_previously_denied_paths", "restored_policy_refuses_again",
)
# These values come from the fixture's fixed increments and the five existing
# SLO objectives: 10%/.1%, 5%/1%, 20%/.5%, 30%/1%, 15%/.1%.
BURNS = {("api-gateway", "availability"): 100.0,
         ("api-gateway", "query-latency"): 5.0,
         ("risk-engine", "recompute-success"): 40.0,
         ("risk-engine", "recompute-latency"): 30.0,
         ("event-bus", "delivery-success"): 150.0}
BUDGETS = dict(zip(BURNS, (0.001, 0.01, 0.005, 0.01, 0.001)))
SETUP_SPEC = importlib.util.spec_from_file_location("grafana_cluster_setup", ROOT / "tools/feed-identity/proof.py")
SETUP = importlib.util.module_from_spec(SETUP_SPEC)
SETUP_SPEC.loader.exec_module(SETUP)


class ProofError(ValueError):
    """A bounded, non-secret diagnostic suitable for the receipt."""


def require(condition, reason):
    if not condition:
        raise ProofError(reason)


def command(args, data=None, timeout=120):
    result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout, cwd=ROOT)
    require(result.returncode == 0, "command_failed:" + Path(args[0]).name)
    require(len(result.stdout) <= 2 * 1024 * 1024, "command_output_exceeded_bound")
    return result.stdout.strip()


def docs(path):
    return [item for item in yaml.safe_load_all(path.read_text(encoding="utf-8")) if item]


def input_hashes():
    return {str((OBS / path).relative_to(ROOT)): hashlib.sha256((OBS / path).read_bytes()).hexdigest()
            for path in INPUTS}


def panel_targets(dashboard):
    result = []
    for panel in dashboard["panels"]:
        for target in panel.get("targets", []):
            require(target["datasource"]["uid"] == "kanz-prometheus", "unexpected_panel_datasource")
            result.append((panel["id"], target["refId"], target["expr"]))
    require(len(result) == 11 and len({(p, r) for p, r, _ in result}) == 11, "panel_inventory_changed")
    return result


def vector(body, keys=None):
    require(body.get("status") == "success" and body["data"]["resultType"] == "vector", "query_not_vector")
    result = {}
    for item in body["data"]["result"]:
        key = (item["metric"].get("service"), item["metric"].get("slo"))
        require(key not in result, "duplicate_query_identity")
        result[key] = float(item["value"][1])
    require(set(result) == (set(BURNS) if keys is None else set(keys)), "empty_or_incomplete_query")
    require(all(math.isfinite(value) for value in result.values()), "nonfinite_query")
    return result


def oracle(expr, values):
    if expr == "slo:budget_window_coverage:ratio":
        # A new TSDB cannot supply a 28d history. Keep that refusal visible.
        require(set(values) == {(None, None)} and all(0 <= value < 0.001 for value in values.values()),
                "fabricated_history_coverage")
        return "fresh_tsdb_incomplete_28d_history"
    require(set(values) == set(BURNS), "incomplete_oracle_input")
    expected = BUDGETS if expr == "slo:error_budget:ratio" else BURNS
    if "slo:error_budget_consumed:ratio" in expr:
        expected = {key: -1.0 for key in BURNS}
    require(all(math.isclose(values[key], expected[key], abs_tol=1e-7, rel_tol=1e-7)
                for key in expected), "panel_disagrees_with_independent_fixture")
    return "incomplete_history_refused" if "consumed" in expr else "independent_counter_ratio"


def same_values(left, right):
    require(set(left) == set(right) and all(math.isclose(left[k], right[k], abs_tol=1e-9, rel_tol=1e-9)
                                          for k in left), "datasource_differs_from_prometheus")


def grafana_frames(body, keys=None):
    result = body.get("results", {}).get("A", {})
    require(not result.get("error") and result.get("status", 200) == 200, "grafana_backend_query_failed")
    found = {}
    for frame in result.get("frames", []):
        for index, field in enumerate(frame["schema"]["fields"]):
            if field["type"] != "number":
                continue
            labels = field.get("labels", {})
            key = (labels.get("service"), labels.get("slo"))
            values = frame["data"]["values"][index]
            require(len(values) == 1 and values[0] is not None and key not in found, "ambiguous_grafana_frame")
            found[key] = float(values[0])
    require(set(found) == (set(BURNS) if keys is None else set(keys)) and
            all(math.isfinite(v) for v in found.values()), "empty_grafana_frames")
    return found


def request(port, path, password=None, body=None):
    headers = {}
    if password:
        headers["Authorization"] = "Basic " + base64.b64encode(("admin:" + password).encode()).decode()
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request("http://127.0.0.1:" + str(port) + path,
                                 data=None if body is None else json.dumps(body).encode(), headers=headers)
    # The endpoint is fixed loopback. Disable ambient proxies and redirects.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args, **_kwargs):
            return None
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(req, timeout=10) as response:
        raw = response.read(2 * 1024 * 1024 + 1)
        require(len(raw) <= 2 * 1024 * 1024, "http_output_exceeded_bound")
        return json.loads(raw)


def eventually(operation, timeout=120):
    end = time.monotonic() + timeout
    while True:
        try:
            return operation()
        except (ProofError, urllib.error.URLError, TimeoutError):
            if time.monotonic() >= end:
                raise ProofError("bounded_observation_timeout") from None
            time.sleep(3)


@contextlib.contextmanager
def forward(kargs, service, remote, scratch):
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    with (scratch / (service + "-forward.txt")).open("w", encoding="utf-8") as output:
        process = subprocess.Popen([*kargs, "-n", NS, "port-forward", "--address", "127.0.0.1",
                                    "service/" + service, str(port) + ":" + str(remote)],
                                   stdout=output, stderr=subprocess.STDOUT)
        try:
            path = "/api/health" if service == "grafana" else "/api/v1/query?query=vector(1)"
            eventually(lambda: request(port, path), timeout=40)
            yield port
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)


EXPORTER = r'''import http.server
import time
started = time.monotonic()
increments = {
    'kanz_gateway_requests_total{code="200"}': 90,
    'kanz_gateway_requests_total{code="500"}': 10,
    'kanz_gateway_request_duration_seconds_bucket{le="0.5"}': 95,
    'kanz_gateway_request_duration_seconds_bucket{le="+Inf"}': 100,
    'kanz_gateway_request_duration_seconds_count': 100,
    'kanz_gateway_request_duration_seconds_sum': 20,
    'kanz_risk_recompute_total{status="ok"}': 80,
    'kanz_risk_recompute_total{status="error"}': 20,
    'kanz_risk_recompute_duration_seconds_bucket{le="0.5"}': 70,
    'kanz_risk_recompute_duration_seconds_bucket{le="+Inf"}': 100,
    'kanz_risk_recompute_duration_seconds_count': 100,
    'kanz_risk_recompute_duration_seconds_sum': 40,
    'kanz_bus_consume_total{result="ok"}': 85,
    'kanz_bus_consume_total{result="error"}': 15,
}
class Metrics(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != '/metrics':
            self.send_error(404)
            return
        n = int(time.monotonic() - started) + 1
        body = ''.join(f'{metric} {increment*n}\n' for metric, increment in increments.items()).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain; version=0.0.4; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_args):
        pass
http.server.HTTPServer(('0.0.0.0', 8080), Metrics).serve_forever()
'''


def fixture_pod(name, labels, script="sleep 3600", port=None, scrape=False):
    metadata = {"name": name, "namespace": NS, "labels": labels}
    if scrape:
        metadata["annotations"] = {"prometheus.io/scrape": "true", "prometheus.io/port": str(port)}
    container = {"name": "fixture", "image": BUSYBOX, "command": ["sh", "-c", script],
                 "resources": {"requests": {"cpu": "10m", "memory": "16Mi"},
                               "limits": {"cpu": "100m", "memory": "64Mi"}},
                 "securityContext": {"readOnlyRootFilesystem": True, "allowPrivilegeEscalation": False,
                                     "capabilities": {"drop": ["ALL"]}},
                 "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"}]}
    if port:
        container["ports"] = [{"containerPort": port}]
    return {"apiVersion": "v1", "kind": "Pod", "metadata": metadata, "spec": {
        "automountServiceAccountToken": False, "securityContext": {"runAsNonRoot": True,
            "runAsUser": 65534, "runAsGroup": 65534, "fsGroup": 65534,
            "seccompProfile": {"type": "RuntimeDefault"}}, "containers": [container],
        "volumes": [{"name": "tmp", "emptyDir": {"sizeLimit": "16Mi"}}]}}


def check_missing(pods, events):
    require(len(pods) == 1, "missing_secret_pod_inventory")
    pod = pods[0]
    require(pod["status"].get("phase") == "Pending" and
            not any(s.get("containerID") for s in pod["status"].get("containerStatuses", [])),
            "missing_secret_did_not_prevent_start")
    uid = pod["metadata"]["uid"]
    require(any(e.get("involvedObject", {}).get("uid") == uid and e.get("reason") == "FailedMount" and
                "grafana-admin" in e.get("message", "") for e in events), "no_owned_secret_mount_refusal")
    return uid


def image_identity(pod, container_name, expected):
    spec = next(c for c in pod["spec"]["containers"] if c["name"] == container_name)
    status = next(c for c in pod["status"]["containerStatuses"] if c["name"] == container_name)
    require(spec["image"] == expected and status.get("ready") is True and
            status.get("restartCount") == 0 and "running" in status.get("state", {}), "wrong_running_image")
    require(re.search(r"sha256:[0-9a-f]{64}$", status.get("imageID", "")), "missing_loaded_image_digest")
    return {"podUID": pod["metadata"]["uid"], "declaredImage": expected, "loadedImageID": status["imageID"]}


def validate_report(report, source, tree):
    require(report.get("schema") == 1 and report.get("source") == source and report.get("tree") == tree,
            "receipt_source_mismatch")
    require(report.get("verdict") == "PASS" and report.get("cleanup") == "verified_absent", "incomplete_receipt")
    require(re.fullmatch(r"cni-proof-[0-9a-f]{12}", report.get("cluster", "")), "unowned_cluster")
    require(report.get("cases") == [{"id": name, "verdict": "PASS"} for name in CASES], "incomplete_case_inventory")
    require(report.get("inputs") == input_hashes() and report.get("panels") and len(report["panels"]) == 11,
            "receipt_input_or_panel_inventory")
    require(set(report.get("images", {})) == {"grafana", "prometheus"}, "receipt_image_inventory")
    for app in ("grafana", "prometheus"):
        declared = next(d for d in docs(OBS / (app + ".yaml")) if d["kind"] == "Deployment")["spec"]["template"]["spec"]["containers"][0]["image"]
        observed = report["images"][app]
        require(observed.get("declaredImage") == declared and observed.get("podUID") and
                re.search(r"sha256:[0-9a-f]{64}$", observed.get("loadedImageID", "")), "receipt_image_changed")
    targets = panel_targets(json.loads((OBS / "dashboards/slo-burn-rate.json").read_text(encoding="utf-8")))
    for actual, (panel, ref, expr) in zip(report["panels"], targets):
        require(actual.get("panel") == panel and actual.get("ref") == ref and
                actual.get("expressionSHA256") == hashlib.sha256(expr.encode()).hexdigest() and
                actual.get("series") == (1 if "coverage:ratio" in expr else 5), "receipt_panel_changed")
        values = {(item.get("service"), item.get("slo")): item["value"] for item in actual.get("values", [])}
        require(len(values) == len(actual.get("values", [])) and actual.get("oracle") == oracle(expr, values),
                "receipt_oracle_changed")
    require(str(report.get("run", "")).isdigit() and str(report.get("attempt", "")).isdigit() and
            report.get("missingSecretPodUID"), "receipt_run_or_missing_secret_identity")


def main():
    source = command(["git", "rev-parse", "HEAD"])
    tree = command(["git", "rev-parse", "HEAD^{tree}"])
    require(not command(["git", "status", "--porcelain", "--untracked-files=all"]), "dirty_source")
    name = "cni-proof-" + uuid.uuid4().hex[:12]
    report = {"schema": 1, "source": source, "tree": tree, "cluster": name,
              "run": os.environ["GITHUB_RUN_ID"], "attempt": os.environ["GITHUB_RUN_ATTEMPT"],
              "inputs": input_hashes(), "dependencies": {"node": SETUP.NODE, "probe": BUSYBOX,
                  "exporter": PYTHON,
                  "installerSHA256": hashlib.sha256((ROOT / "tools/feed-identity/install.sh").read_bytes()).hexdigest()},
              "cases": [], "images": {}, "verdict": "FAIL", "cleanup": "unknown"}
    artifact = Path(os.environ["RUNNER_TEMP"]) / "grafana-kubernetes.json"
    def passed(case):
        require(case == CASES[len(report["cases"])], "case_order_changed")
        report["cases"].append({"id": case, "verdict": "PASS"})
    with tempfile.TemporaryDirectory(prefix="kanz-grafana-") as directory:
        scratch = Path(directory)
        kargs = ["kubectl", "--kubeconfig", str(scratch / "kubeconfig"), "--context", "kind-" + name]
        def k(*args, data=None, timeout=120):
            return command([*kargs, *args], data, timeout)
        def apply(items):
            k("apply", "-f", "-", data=yaml.safe_dump_all(items))
        def items(kind, namespace=NS, label=None):
            args = ["-n", namespace, "get", kind, "-o", "json"]
            if label:
                args += ["-l", label]
            return json.loads(k(*args))["items"]
        def only_pod(app, namespace=NS):
            pods = items("pods", namespace, "app=" + app)
            require(len(pods) == 1, "ambiguous_running_pod")
            return pods[0]
        try:
            SETUP.create_enforcing_cluster(name, scratch)
            mode = k("-n", "kube-system", "exec", "daemonset/cilium", "-c", "cilium-agent", "--", "cilium-dbg", "config")
            require(re.findall(r"(?m)^PolicyEnforcement\s*:\s*(\S+)\s*$", mode) == ["default"], "not_enforcing_cilium")
            report["dependencies"]["ciliumLoadedImages"] = [c["imageID"] for p in items("pods", "kube-system", "k8s-app=cilium")
                for c in p["status"]["containerStatuses"]]
            require(report["dependencies"]["ciliumLoadedImages"] and all(re.search(r"sha256:[0-9a-f]{64}$", image)
                    for image in report["dependencies"]["ciliumLoadedImages"]), "cilium_loaded_image_identity_missing")
            passed("loaded_enforcing_cilium")
            apply([{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": ns}}
                   for ns in (NS, MISSING_NS)])
            grafana = docs(OBS / "grafana.yaml")
            dashboard = json.loads((OBS / "dashboards/slo-burn-rate.json").read_text(encoding="utf-8"))
            targets = panel_targets(dashboard)
            def dashboard_cm(namespace):
                return {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "grafana-dashboards", "namespace": namespace},
                        "data": {"slo-burn-rate.json": (OBS / "dashboards/slo-burn-rate.json").read_text(encoding="utf-8")}}
            missing = copy.deepcopy(grafana)
            for d in missing:
                d["metadata"]["namespace"] = MISSING_NS
            apply([*missing, dashboard_cm(MISSING_NS)])
            missing_uid = eventually(lambda: check_missing(items("pods", MISSING_NS, "app=grafana"), items("events", MISSING_NS)))
            report["missingSecretPodUID"] = missing_uid
            passed("missing_admin_secret_refuses_startup")
            k("delete", "namespace", MISSING_NS, "--wait=true", "--timeout=90s")
            password = secrets.token_urlsafe(32)
            apply([{"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "grafana-admin", "namespace": NS},
                    "type": "Opaque", "data": {"admin-password": base64.b64encode(password.encode()).decode()}}])
            apply([*grafana, dashboard_cm(NS), *docs(OBS / "prometheus.yaml"), *docs(OBS / "rules-configmap.yaml")])
            for app in ("grafana", "prometheus"):
                k("-n", NS, "rollout", "status", "deployment/" + app, "--timeout=180s", timeout=190)
                declared = next(d for d in (grafana if app == "grafana" else docs(OBS / "prometheus.yaml"))
                                if d["kind"] == "Deployment")["spec"]["template"]["spec"]["containers"][0]["image"]
                report["images"][app] = image_identity(only_pod(app), app, declared)
                passed("committed_" + app + "_ready")
            pod = only_pod("grafana")["metadata"]["name"]
            actual_pod = only_pod("grafana")
            require(actual_pod["spec"]["securityContext"]["fsGroup"] == 472 and
                    next(c for c in actual_pod["spec"]["containers"] if c["name"] == "grafana")
                    ["securityContext"]["readOnlyRootFilesystem"] is True, "grafana_filesystem_posture_changed")
            paths = {"/etc/grafana/provisioning/datasources/datasources.yaml": next(d for d in grafana if d["metadata"]["name"] == "grafana-datasources")["data"]["datasources.yaml"],
                     "/etc/grafana/provisioning/dashboards/dashboards.yaml": next(d for d in grafana if d["metadata"]["name"] == "grafana-dashboard-provider")["data"]["dashboards.yaml"],
                     "/var/lib/grafana/dashboards/slo-burn-rate.json": dashboard_cm(NS)["data"]["slo-burn-rate.json"]}
            for path, content in paths.items():
                digest = k("-n", NS, "exec", pod, "--", "sha256sum", path).split()[0]
                require(digest == hashlib.sha256(content.encode()).hexdigest(), "public_mount_changed")
            k("-n", NS, "exec", pod, "--", "sh", "-ec",
              'test "$(id -u)" = 472; test -w /var/lib/grafana; test -w /tmp; '
              'test ! -w /etc/grafana/provisioning/datasources/datasources.yaml; test -r /etc/grafana/secrets/admin-password')
            passed("exact_public_mounts_and_permissions")
            exporter = fixture_pod("slo-fixture", {"app": "slo-fixture"}, EXPORTER, 8080, True)
            exporter["spec"]["containers"][0].update(image=PYTHON, command=["python3", "-B", "-c", EXPORTER])
            other = fixture_pod("other", {"app": "other"}, "mkdir -p /tmp/www; echo other > /tmp/www/index.html; exec httpd -f -p 9090 -h /tmp/www", 9090)
            untrusted = fixture_pod("untrusted", {"app": "untrusted"})
            apply([exporter, other, untrusted,
                   {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "other", "namespace": NS},
                    "spec": {"selector": {"app": "other"}, "ports": [{"port": 9090, "targetPort": 9090}]}}])
            k("-n", NS, "wait", "--for=condition=Ready", "pod/slo-fixture", "pod/other", "pod/untrusted", "--timeout=90s")
            with forward(kargs, "grafana", 3000, scratch) as gp, forward(kargs, "prometheus", 9090, scratch) as pp:
                try:
                    request(gp, "/api/dashboards/uid/kanz-slo-burn")
                    raise ProofError("anonymous_dashboard_disclosed")
                except urllib.error.HTTPError as exc:
                    require(exc.code in (401, 403), "unexpected_anonymous_response")
                passed("unauthenticated_dashboard_refused")
                datasource = request(gp, "/api/datasources/uid/kanz-prometheus", password)
                require(datasource["url"] == "http://prometheus.kanz-observability.svc:9090" and
                        datasource["type"] == "prometheus" and datasource["readOnly"] is True, "datasource_not_provisioned")
                loaded = request(gp, "/api/dashboards/uid/kanz-slo-burn", password)
                require(loaded["meta"]["provisioned"] and panel_targets(loaded["dashboard"]) == targets,
                        "dashboard_not_committed_provisioning")
                passed("provisioned_datasource_and_dashboard")
                def ready_rates():
                    value = vector(request(pp, "/api/v1/query?" + urllib.parse.urlencode({"query": targets[0][2]})))
                    oracle(targets[0][2], value)
                eventually(ready_rates, timeout=240)
                timestamp = int(time.time())
                report["queryTime"] = timestamp
                report["panels"] = []
                for panel, ref, expr in targets:
                    keys = {(None, None)} if expr == "slo:budget_window_coverage:ratio" else set(BURNS)
                    params = urllib.parse.urlencode({"query": expr, "time": str(timestamp)})
                    direct = vector(request(pp, "/api/v1/query?" + params), keys)
                    proxy = vector(request(gp, "/api/datasources/proxy/uid/kanz-prometheus/api/v1/query?" + params, password), keys)
                    same_values(direct, proxy)
                    queried = grafana_frames(request(gp, "/api/ds/query", password, {"queries": [{"refId": "A",
                        "datasource": {"type": "prometheus", "uid": "kanz-prometheus"}, "expr": expr,
                        "instant": True, "range": False, "format": "time_series", "intervalMs": 30000,
                        "maxDataPoints": 1000}], "from": str((timestamp - 300) * 1000), "to": str(timestamp * 1000)}), keys)
                    same_values(direct, queried)
                    outcome = oracle(expr, direct)
                    report["panels"].append({"panel": panel, "ref": ref, "expressionSHA256": hashlib.sha256(expr.encode()).hexdigest(),
                                              "series": len(direct), "oracle": outcome,
                                              "values": [{"service": key[0], "slo": key[1], "value": value}
                                                         for key, value in direct.items()]})
                passed("nonempty_panel_queries_match_prometheus_and_oracle")
                passed("grafana_to_prometheus_allowed")
            def reachable(source_pod, url):
                result = subprocess.run([*kargs, "-n", NS, "exec", source_pod, "--", "sh", "-c",
                    'wget -q -T 3 -O /dev/null "$1"', "probe", url], capture_output=True, text=True, timeout=12)
                if result.returncode == 0:
                    return True
                # Require a network-level timeout, not a missing tool or exec failure.
                require("timed out" in result.stderr.lower(), "probe_failed_without_network_timeout")
                return False
            other_url = "http://other." + NS + ".svc:9090/"
            grafana_url = "http://grafana." + NS + ".svc:3000/api/health"
            eventually(lambda: require(not reachable(pod, other_url), "grafana_egress_not_denied"), 40)
            passed("grafana_to_other_9090_denied")
            eventually(lambda: require(not reachable("untrusted", grafana_url), "grafana_ingress_not_denied"), 40)
            passed("untrusted_to_grafana_denied")
            policy = next(d for d in grafana if d["kind"] == "NetworkPolicy")
            k("-n", NS, "delete", "networkpolicy", policy["metadata"]["name"])
            eventually(lambda: require(reachable(pod, other_url) and reachable("untrusted", grafana_url), "policy_control_not_discriminating"), 50)
            passed("removed_policy_allows_previously_denied_paths")
            apply([policy])
            eventually(lambda: require(not reachable(pod, other_url) and not reachable("untrusted", grafana_url), "restored_policy_not_enforced"), 50)
            passed("restored_policy_refuses_again")
            report["verdict"] = "PASS"
        except (ProofError, OSError, subprocess.TimeoutExpired, KeyError, StopIteration, urllib.error.URLError) as exc:
            report["failure"] = str(exc) if isinstance(exc, ProofError) else type(exc).__name__
            report["nextCase"] = CASES[len(report["cases"])] if len(report["cases"]) < len(CASES) else "receipt"
            report["verdict"] = "FAIL"
        finally:
            try:
                command(["kind", "delete", "cluster", "--name", name, "--kubeconfig", str(scratch / "kubeconfig")], timeout=180)
                require(name not in command(["kind", "get", "clusters"]).splitlines(), "cluster_still_present")
                report["cleanup"] = "verified_absent"
            except (ProofError, OSError, subprocess.TimeoutExpired):
                report["verdict"] = "FAIL"
            if report["verdict"] == "PASS":
                try:
                    validate_report(report, source, tree)
                except (ProofError, KeyError, TypeError):
                    report["verdict"] = "FAIL"
                    report["failure"] = "receipt_validation_failed"
            artifact.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, separators=(",", ":")))
    return 0 if report["verdict"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
