"""Three independent socket observations; refusal/reset is never a policy denial."""
import json
import socket
import sys
import time


def observe(host, port, expected):
    results = []
    for _ in range(3):
        try:
            connection = socket.create_connection((host, port), timeout=1.5)
        except TimeoutError:
            result = 'blocked'
        else:
            with connection:
                connection.settimeout(1.5)
                connection.sendall(b'GET /fixture HTTP/1.0\r\nHost: fixture\r\nX-Kanz-Principal-Tenant: synthetic\r\n\r\n')
                response = b''
                while True:
                    block = connection.recv(1024)
                    if not block:
                        break
                    response += block
                    if len(response) > 4096:
                        raise ValueError('oversized synthetic response')
                if (not response.startswith(b'HTTP/1.0 200 ')
                        or response.split(b'\r\n\r\n', 1)[-1] != ('fixture-port=' + str(port) + '\n').encode()):
                    raise ValueError('invalid synthetic response')
                result = 'allowed'
        if result != expected:
            raise ValueError('unexpected network observation')
        results.append(result)
        time.sleep(0.2)
    return results


if __name__ == '__main__':
    print(json.dumps(observe(sys.argv[1], int(sys.argv[2]), sys.argv[3])))
