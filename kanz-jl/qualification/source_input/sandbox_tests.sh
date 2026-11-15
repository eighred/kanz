#!/usr/bin/env bash
# Trusted orchestration only; fixture processing runs in an empty private root.
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
stage="/tmp/kanz-openssl-tests-$(cat /proc/sys/kernel/random/uuid)"
cleanup() {
  # This exclusive parent-owned source copy is never writable in the sandbox.
  for file in produce.py test_produce.py pins.json VERSION.dat; do
    [[ ! -e $stage/$file ]] || rm -- "$stage/$file"
  done
  [[ ! -d $stage ]] || rmdir -- "$stage"
}
trap cleanup EXIT
python3 -I -B "$here/produce.py" --stage-tests "$stage"
command=(/usr/bin/python3 -B -m unittest discover -s /input -p test_produce.py -v)
if [[ ${1:-} == --probe ]]; then
  command=(/usr/bin/true)
elif [[ $# != 0 ]]; then
  exit 2
fi
sudo systemd-run --quiet --wait --pipe --collect \
  --unit="kanz-openssl-tests-$(cat /proc/sys/kernel/random/uuid)" \
  --uid=nobody --gid=nogroup \
  --property=MemoryMax=512M --property=MemorySwapMax=0 \
  --property=TasksMax=64 --property=CPUQuota=100% --property=RuntimeMaxSec=90 \
  --property=LimitNOFILE=64 --property=LimitFSIZE=1048576 --property=LimitNPROC=64 --property=LimitCPU=60 \
  --property=NoNewPrivileges=yes \
  /usr/bin/bwrap --unshare-all --unshare-user --die-with-parent --new-session --clearenv \
  --disable-userns --assert-userns-disabled --cap-drop ALL \
  --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/lib /lib --symlink usr/lib64 /lib64 \
  --proc /proc --dev /dev --dir /etc --ro-bind /etc/ld.so.cache /etc/ld.so.cache \
  --size 33554432 --perms 0777 --tmpfs /work --ro-bind "$stage" /input \
  --remount-ro / --chdir /input --setenv PATH /usr/bin:/bin \
  --setenv HOME /work --setenv TMPDIR /work --setenv LANG C --setenv LC_ALL C \
  "${command[@]}"
