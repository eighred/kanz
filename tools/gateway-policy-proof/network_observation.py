"""Closed socket classification shared by synthetic CNI and workload probes.

The source is sent to isolated Python processes. Callers retain their own echo,
HTTP or TCP-only protocol; a connect error never proves protocol health.
"""

CONNECT_SOURCE = """import errno,ipaddress,socket
def connect_literal(address,port,timeout):
 if not isinstance(address,str) or '%' in address:
  raise ValueError('literal address required')
 ipaddress.ip_address(address)
 if type(port) is not int or port not in (8080,8083,8091,8095,8101,8102):
  raise ValueError('port outside probe contract')
 if timeout not in (1,1.5,2):
  raise ValueError('connect timeout outside probe contract')
 try:
  connection=socket.create_connection((address,port),timeout=timeout)
 except TimeoutError:
  return None,'timeout'
 except OSError as error:
  if error.errno==errno.ECONNREFUSED: return None,'refused'
  if error.errno==errno.EHOSTUNREACH: return None,'host_unreachable'
  return None,'unknown'
 return connection,'connected'
"""

DENIALS = frozenset(("timeout", "refused", "host_unreachable"))
OUTCOMES = DENIALS | {"connected", "unknown"}


def batch(value):
    if not isinstance(value, list) or len(value) != 3 or any(
            not isinstance(item, str) or item not in OUTCOMES for item in value):
        raise ValueError("invalid connection batch")
    return value


TCP_BATCH = CONNECT_SOURCE + """import json,sys,time
values=[]
for index in range(3):
 if index: time.sleep(.2)
 connection,result=connect_literal(sys.argv[1],int(sys.argv[2]),1)
 if connection is not None: connection.close()
 values.append(result)
print(json.dumps(values,separators=(',',':')))
"""
