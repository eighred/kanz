"""Synthetic HTTP listener: no domain state, credentials or identity authority."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import sys
import threading


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        body = ('fixture-port=' + str(self.server.server_port) + '\n').encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == '__main__':
    servers = [ThreadingHTTPServer(('0.0.0.0', int(port)), Handler) for port in sys.argv[1:]]
    for server in servers:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    threading.Event().wait()
