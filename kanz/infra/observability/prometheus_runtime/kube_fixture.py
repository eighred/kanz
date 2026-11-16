"""Disposable TLS Kubernetes pod discovery endpoint; no cluster credentials."""

import http.server
import json
import ssl
import sys
import urllib.parse
import urllib.request


class Pods(http.server.BaseHTTPRequestHandler):
    initial_events_requested = False
    initial_bookmark_sent = False

    def do_GET(self):
        if self.path == "/fixture-status":
            body = json.dumps({"initial_events_requested": Pods.initial_events_requested,
                               "initial_bookmark_sent": Pods.initial_bookmark_sent}).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if urllib.parse.urlsplit(self.path).path != "/api/v1/pods":
            self.send_error(404)
            return
        pod = {
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": "prometheus", "namespace": "qualification",
                         "uid": "fixture-pod", "resourceVersion": "1"},
            "spec": {"containers": [{"name": "prometheus", "ports": [
                {"name": "http", "containerPort": 9090, "protocol": "TCP"}]}]},
            "status": {"phase": "Running", "podIP": "127.0.0.1"},
        }
        params = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
        if params.get("watch") == ["true"]:
            body = json.dumps({"type": "ADDED", "object": pod}).encode() + b"\n"
            if params.get("sendInitialEvents") == ["true"]:
                # Streaming lists end their initial set with this bookmark:
                # kubernetes.io/docs/reference/using-api/api-concepts/#streaming-lists
                Pods.initial_events_requested = True
                bookmark = {"apiVersion": "v1", "kind": "Pod", "metadata": {
                    "resourceVersion": "1", "annotations": {"k8s.io/initial-events-end": "true"}}}
                body += json.dumps({"type": "BOOKMARK", "object": bookmark}).encode() + b"\n"
                Pods.initial_bookmark_sent = True
        else:
            body = json.dumps({"apiVersion": "v1", "kind": "PodList",
                               "metadata": {"resourceVersion": "1"}, "items": [pod]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1] == "serve":
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 8443), Pods)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain("/fixture/server.crt", "/fixture/server.key")
        server.socket = context.wrap_socket(server.socket, server_side=True)
        server.serve_forever()
    elif len(sys.argv) == 2 and sys.argv[1] == "status":
        context = ssl.create_default_context(cafile="/fixture/server.crt")
        with urllib.request.urlopen("https://127.0.0.1:8443/fixture-status",
                                    context=context, timeout=3) as response:
            sys.stdout.write(response.read(4096).decode())
    elif len(sys.argv) == 3 and sys.argv[1] == "get":
        # The caller supplies only fixed loopback Prometheus API paths.
        path = sys.argv[2]
        if not path.startswith(("/-/ready", "/api/v1/query?")):
            raise ValueError("unexpected probe path")
        with urllib.request.urlopen("http://127.0.0.1:9090" + path, timeout=3) as response:
            sys.stdout.write(response.read(1024 * 1024).decode())
    else:
        raise ValueError("expected serve or get")
