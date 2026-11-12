"""Wait for the local smoke services without confusing liveness with readiness."""
import time
import urllib.error
import urllib.request

# Never send a developer's proxy or credentials to a readiness endpoint.
client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
deadline = time.monotonic() + 120
pending = {'risk-engine': 'http://127.0.0.1:8082/readyz',
           'api-gateway': 'http://127.0.0.1:8080/readyz'}
while pending and time.monotonic() < deadline:
    for service, url in list(pending.items()):
        try:
            with client.open(url, timeout=2) as response:
                if response.status == 200:
                    print(f'{service} ready')
                    del pending[service]
        except (OSError, urllib.error.URLError):
            pass
    if pending:
        time.sleep(1)
if pending:
    raise SystemExit('readiness timed out: ' + ', '.join(sorted(pending)))
