"""Cancel-path cleanup accepts only this run's exclusive pre-create claim."""
import json
import os
from pathlib import Path
import re
import stat

from qualify import command


def validate_claim(value, source, run, attempt):
    if not isinstance(value,dict) or set(value)!={'source','run','attempt','cluster'}:
        raise ValueError('claim shape')
    if value['source']!=source or value['run']!=run or value['attempt']!=attempt or not re.fullmatch(r'redpanda-proof-[0-9a-f]{12}',value['cluster']):
        raise ValueError('foreign claim')
    return value['cluster']


def main():
    path=Path(os.environ['RUNNER_TEMP'])/'native-kafka-claim.json'
    if not path.exists(): return
    if os.name!='posix' or not hasattr(os,'O_NOFOLLOW'): raise ValueError('Linux required')
    fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    try:
        before=os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink!=1 or not 0<before.st_size<=1024: raise ValueError('claim custody')
        raw=os.read(fd,1025); after=os.fstat(fd)
        fields=('st_dev','st_ino','st_mode','st_nlink','st_size','st_mtime_ns','st_ctime_ns')
        if any(getattr(before,key)!=getattr(after,key) for key in fields) or len(raw)!=before.st_size: raise ValueError('claim changed')
    finally: os.close(fd)
    name=validate_claim(json.loads(raw),command(['git','rev-parse','HEAD']),os.environ['GITHUB_RUN_ID'],os.environ['GITHUB_RUN_ATTEMPT'])
    if name in command(['kind','get','clusters']).splitlines():
        command(['kind','delete','cluster','--name',name],timeout=180)
    if name in command(['kind','get','clusters']).splitlines(): raise RuntimeError('owned cleanup unconfirmed')


if __name__=='__main__': main()
