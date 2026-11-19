#!/usr/bin/env bash
set -euo pipefail
umask 022
export HOME=/work/home TMPDIR=/work/tmp
mkdir -p "$HOME" "$TMPDIR" /work/build
cd /work/build
triplet=x86_64-linux-gnu
export CC=$triplet-gcc CXX=$triplet-g++ FC=$triplet-gfortran AR=$triplet-ar AS=$triplet-as LD=$triplet-ld NM=$triplet-nm RANLIB=$triplet-ranlib STRIP=$triplet-strip
# The builder is musl. The resulting programs target the vendor's GNU sysroot.
"$CXX" -v 2> /out/toolchain.txt
"$CC" -print-sysroot >> /out/toolchain.txt
"$FC" -dumpfullversion >> /out/toolchain.txt
for compiler in "$CC" "$CXX" "$FC"; do
    [[ "$("$compiler" -dumpfullversion)" == 15.2.0 ]]
    [[ "$("$compiler" -dumpmachine)" == x86_64-linux-gnu ]]
done
cat > /work/hello.cc <<'CPP'
#include <cstdio>
int main() { return std::puts("ordinary compiler control") < 0; }
CPP
"$CXX" -std=c++17 -O2 /work/hello.cc -o /out/toolchain-smoke
export CFLAGS='-O2 -g0 -fPIC' CXXFLAGS='-O2 -g0 -fPIC'
test "$(sha256sum /vendor-release.tar | cut -d' ' -f1)" = f3f407120e0043539367a09147cf0f51bd0d66ecffdd153358b0331a1738598d
tar -xf /vendor-release.tar -C /source --no-same-owner
src=/source/gcc
# Standalone target build stages the sibling libgcc thread header using its original Makefile rule.
mkdir -p /work/libgcc
ln -s /source/gcc/libgcc/gthr-posix.h /work/libgcc/gthr-default.h
"$src/libstdc++-v3/configure" --build=x86_64-linux-musl --host=$triplet --target=$triplet --prefix=/opt/kanz-gcc --disable-multilib --enable-shared --enable-static --enable-libstdcxx-time=yes --disable-werror --disable-nls > /out/configure.log 2>&1 || { tail -35 /out/configure.log; tail -60 config.log > /out/config-failure.txt; exit 7; }
grep -F 'checking for gthreads library... yes' /out/configure.log
make -j3 > /out/build.log 2>&1 || { tail -45 /out/build.log; exit 8; }
make install DESTDIR=/work/install > /out/install.log 2>&1 || { tail -35 /out/install.log; exit 9; }
tar -czf /out/installed-libstdcxx.tar.gz -C /work/install .
library=/work/install/opt/kanz-gcc/lib64/libstdc++.so.6.0.34
test "$(sha256sum "$library" | cut -d' ' -f1)" = 18c8d1e340b784387ca1fdda0d1b90c5fe94842517a6b5151dc71387cbf409b1
cp "$library" /out/
header_dir=/work/install/opt/kanz-gcc/include/c++/15.2.0
target_dir=/work/install/opt/kanz-gcc/include/c++/15.2.0/x86_64-linux-gnu
test -f /work/install/opt/kanz-gcc/include/c++/15.2.0/vector
"$CXX" -std=c++17 -O2 -fopenmp -nostdinc++ -isystem "$header_dir" -isystem "$target_dir" -L"$(dirname "$library")" /cxx-surface.cc -o /out/normal-cxx-controls
