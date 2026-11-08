"""Closed advisory runtime evidence; never retain broker log text."""
import re


def memory_events(raw):
    if not isinstance(raw,str) or len(raw)>4096:
        raise ValueError('memory event bound')
    values={}
    for line in raw.splitlines():
        fields=line.split()
        if len(fields)!=2 or fields[0] in values or not re.fullmatch(r'[0-9]{1,20}',fields[1]):
            raise ValueError('memory event shape')
        values[fields[0]]=int(fields[1])
    keys=('oom','oom_kill','oom_group_kill')
    if any(key not in values for key in keys):
        raise ValueError('memory event incomplete')
    return {key:values[key] for key in keys}


def broker_log(raw):
    if not isinstance(raw,str) or len(raw.encode())>32768:
        raise ValueError('broker log bound')
    return {'allocationFailure':any(marker in raw for marker in ('std::bad_alloc','seastar::memory::bad_alloc','std::bad_array_new_length')),
            'assertionFailure': 'Assertion failed' in raw or 'assertion failed' in raw}


def collection_error(error):
    if isinstance(error,ValueError): return 'shape'
    if isinstance(error,RuntimeError): return 'command'
    return 'unexpected'
