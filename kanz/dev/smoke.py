"""Prove the local seed reaches the authenticated risk read surface."""
import json
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
URL = 'http://127.0.0.1:8080/v1/portfolios/PF1/exposure'
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


client = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
try:
    client.open(URL, timeout=3).close()
except urllib.error.HTTPError as exc:
    if exc.code != 401:
        raise SystemExit(f'expected unauthenticated refusal, got {exc.code}')
else:
    raise SystemExit('unauthenticated exposure request was accepted')

# This is the repository's explicit public local-development identity only.
# Capture it in memory; never print the bearer or write it to an evidence file.
token = subprocess.check_output(
    ['go', 'run', './cmd/kanz-devtoken', '--secret', 'dev-secret', '--tenant', 'dev',
     '--role', 'kanz-user', '--portfolio', 'PF1', '--ttl', '5m'],
    cwd=ROOT, text=True, timeout=120).strip()
deadline = time.monotonic() + 30
while time.monotonic() < deadline:
    try:
        request = urllib.request.Request(URL, headers={'Authorization': 'Bearer ' + token})
        with client.open(request, timeout=3) as response:
            result = json.load(response)
        instruments = {item['bucket'] for item in result['set']['exposures']
                       if item['dimension'] == 'EXPOSURE_DIMENSION_INSTRUMENT'}
        if result['portfolio_id'] == 'PF1' and result['owner_tenant'] == 'dev' and instruments == {'AAPL', 'MSFT', 'NVDA', 'TSLA'}:
            print('PASS: unauthenticated refusal; tenant dev/PF1 seed reached the risk read surface (four instruments)')
            break
    except urllib.error.HTTPError as exc:
        if exc.code != 404:
            raise SystemExit(f'exposure request failed: HTTP {exc.code}') from None
    time.sleep(1)
else:
    raise SystemExit('seeded exposure did not become queryable within 30 seconds')
