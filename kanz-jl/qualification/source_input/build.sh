#!/usr/bin/env bash
# Runs only in the producer's network-isolated, resource-bounded container.
set -euo pipefail
umask 022
exec > /work/result/build.log 2>&1
[[ $(id -u) == 65532 && $HOME == /work/home && $SOURCE_DATE_EPOCH == 1794931913 ]]
mkdir /work/home /work/tmp /work/source /work/stage
for tool in gcc make perl readelf sha256sum tar gzip dpkg-query; do
  command -v "$tool"
done
# The parent validates every archive member before this extraction. No downloads
# or package installation are permitted in the build namespace.
tar -xzf /input/source.tar.gz --strip-components=1 -C /work/source
cd /work/source
cmp VERSION.dat /input/VERSION.dat
{
  printf '%s\n' '--- compiler ---'
  gcc --version
  gcc -v 2>&1
  printf '%s\n' '--- tools ---'
  make --version
  perl -V
  ld --version
  printf '%s\n' '--- tool bytes ---'
  for tool in gcc as ld make perl tar gzip readelf; do
    sha256sum "$(readlink -f "$(command -v "$tool")")"
  done
  sha256sum "$(gcc -print-prog-name=cc1)" "$(gcc -print-file-name=crtbeginS.o)" "$(gcc -print-file-name=libgcc_s.so.1)"
  printf '%s\n' '--- sysroot ---'
  gcc -print-sysroot
  cat /etc/os-release
  dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n'
} > /work/result/toolchain.txt
./Configure linux-x86_64 shared --prefix=/opt/kanz-openssl --libdir=lib \
  -ffile-prefix-map=/work/source=/usr/src/openssl \
  -fdebug-prefix-map=/work/source=/usr/src/openssl \
  -fmacro-prefix-map=/work/source=/usr/src/openssl
perl configdata.pm --dump > /work/result/configure.txt
make -j2
make test HARNESS_VERBOSE=1 HARNESS_JOBS=1 > /work/result/full_tests.log 2>&1
make test HARNESS_VERBOSE=1 HARNESS_JOBS=1 TESTS='test_bio_dgram test_ec test_evp test_evp_extra test_quic_stream' \
  > /work/result/regression_tests.log 2>&1
LD_LIBRARY_PATH=/work/source apps/openssl version -a > /work/result/version.txt
for name in libcrypto.so.3 libssl.so.3; do
  [[ -f $name && ! -L $name ]]
  cp -- "$name" "/work/stage/$name"
done
cp -- LICENSE.txt /work/stage/LICENSE.txt
tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
  --format=ustar -cf /work/result/native.tar -C /work/stage LICENSE.txt libcrypto.so.3 libssl.so.3
gzip -n /work/result/native.tar
cp -- /work/stage/libcrypto.so.3 /work/result/libcrypto.so.3
cp -- /work/stage/libssl.so.3 /work/result/libssl.so.3
cp -- LICENSE.txt /work/result/LICENSE.txt
# No success marker is consumed. The parent independently checks process exit,
# required upstream recipe results, byte equality, archive members and ELF data.
