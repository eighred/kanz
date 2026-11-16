"""Wait for the two fixed local readiness endpoints; never follow redirects."""
import time
import urllib.error
import urllib.request

ENDPOINTS = {'risk-engine': 'http://127.0.0.1:8082/readyz',
             'api-gateway': 'http://127.0.0.1:8080/readyz'}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def wait_for_services(client=None):
    # The caller cannot provide endpoints. Disable ambient proxy configuration
    # and redirects, including redirects to another loopback port.
    if client is None:
        client = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    deadline = time.monotonic() + 120
    pending = dict(ENDPOINTS)
    while pending and time.monotonic() < deadline:
        for service, url in list(pending.items()):
            try:
                with client.open(url, timeout=2) as response:
                    if response.geturl() != url:
                        raise ValueError('readiness response left its fixed local endpoint')
                    if response.status == 200:
                        print(f'{service} ready')
                        del pending[service]
            except urllib.error.HTTPError as exc:
                if 300 <= exc.code < 400:
                    raise ValueError('readiness redirect refused') from None
            except (OSError, urllib.error.URLError):
                pass
        if pending:
            time.sleep(1)
    if pending:
        raise ValueError('readiness timed out: ' + ', '.join(sorted(pending)))


if __name__ == '__main__':
    try:
        wait_for_services()
    except ValueError as exc:
        raise SystemExit(str(exc)) from None
