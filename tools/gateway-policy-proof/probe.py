"""Three independent observations; connected sockets still require exact HTTP."""
import json
import socket
import sys
import time
from network_observation import CONNECT_SOURCE, DENIALS

exec(compile(CONNECT_SOURCE, "<closed-connect-source>", "exec"))


def observe(host, port, expected):
    results = []
    for _ in range(3):
        connection, category = connect_literal(host, port, 1.5)
        if category in DENIALS:
            result = 'blocked'
        elif category == 'connected':
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
        else:
            raise ValueError('unknown connection observation')
        results.append(result)
        time.sleep(0.2)
    return results


if __name__ == '__main__':
    print(json.dumps(observe(sys.argv[1], int(sys.argv[2]), sys.argv[3])))
