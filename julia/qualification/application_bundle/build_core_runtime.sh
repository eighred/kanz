#!/bin/sh
set -eu
cd /tmp
test ! -e julia-1.13.3
tar -xzf /input/julia-1.13.3.tar.gz --no-same-owner
cd julia-1.13.3
mkdir -p usr/include usr/lib usr/bin
tar -xf /input/native-relink-inputs.tar --no-same-owner -C usr
cp -R /input/core-libraries/include/. usr/include/
cp -R /input/core-libraries/lib/. usr/lib/
mkdir /tmp/utf8
cp /input/utf8proc/* /tmp/utf8/
make -C /tmp/utf8 CC=x86_64-linux-gnu-gcc CFLAGS=-O2 PICFLAG=-fPIC AR=x86_64-linux-gnu-ar libutf8proc.a >/out/utf8proc.log 2>&1
test "$(sha256sum /tmp/utf8/libutf8proc.a | cut -d' ' -f1)" = 6d4712088ca7bc5b71bd346fe20b3027414ae4334a7900e902620559b3938703
cp /tmp/utf8/libutf8proc.a usr/lib/
cp /tmp/utf8/utf8proc.h usr/include/
cd usr/lib
ln -s libunwind.so.8.2.0 libunwind.so.8
ln -s libunwind.so.8 libunwind.so
ln -s libzstd.so.1.5.7 libzstd.so.1
ln -s libzstd.so.1 libzstd.so
ln -s julia/libz.so.1.3.2 libz.so
ln -s julia/libz.so.1.3.2 libz.so.1
cd /tmp/julia-1.13.3
prefix="$PWD/usr"
sys=/opt/x86_64-linux-gnu/x86_64-linux-gnu/sys-root
cat >/tmp/kanz-gnu-llvm-config <<EOF
#!/bin/sh
export LD_LIBRARY_PATH="$prefix/lib/julia:$prefix/lib:$sys/lib64:$sys/usr/lib64"
exec "$prefix/tools/llvm-config" "\$@"
EOF
chmod 755 /tmp/kanz-gnu-llvm-config
/tmp/kanz-gnu-llvm-config --version
/tmp/kanz-gnu-llvm-config --includedir
/tmp/kanz-gnu-llvm-config --libs support targetparser --system-libs --link-static
export CC=x86_64-linux-gnu-gcc CXX=x86_64-linux-gnu-g++ FC=x86_64-linux-gnu-gfortran AR=x86_64-linux-gnu-ar RANLIB=x86_64-linux-gnu-ranlib LD=x86_64-linux-gnu-ld
runmake() { make -j2 VERBOSE=1 CC="$CC" CXX="$CXX" FC="$FC" AR="$AR" RANLIB="$RANLIB" LD="$LD" LLVM_CONFIG_HOST=/tmp/kanz-gnu-llvm-config LLVM_CONFIG=/tmp/kanz-gnu-llvm-config CPPFLAGS=-I/headers "$@"; }
runmake -C src/support libsupport.a >/out/owner-support.log 2>&1
runmake -C src julia.expmap >/out/owner-expmap.log 2>&1
gnu_lib=$(dirname "$("$CC" -print-file-name=libatomic.so.1)")
runmake -C src/flisp LDFLAGS="-Wl,-rpath,$gnu_lib:$sys/lib64:$sys/usr/lib64:$prefix/lib:$prefix/lib/julia" flisp >/out/owner-flisp.log 2>&1
runmake -C src LDFLAGS="-Wl,-Map,/out/libjulia-internal.linkmap" libjulia-internal-release >/out/owner-full.log 2>&1 || { tail -18 /out/owner-full.log ; exit 6; }
if grep -E 'error while loading shared libraries|not found|Segmentation fault' /out/owner-full.log > /out/generator-diagnostics.txt; then
    cat /out/generator-diagnostics.txt
    exit 7
fi
test -s src/julia_flisp.boot
cp src/julia_flisp.boot /out/
test -s /out/libjulia-internal.linkmap
grep -F "/usr/lib/libstdc++.a(new_opa.o)" /out/libjulia-internal.linkmap >/out/aligned-allocation-source.txt
cp -L usr/lib/libjulia-internal.so.1.13 /out/libjulia-internal.so.1.13.3
patchelf --set-rpath '$ORIGIN:$ORIGIN/..' /out/libjulia-internal.so.1.13.3
test "$(sha256sum /out/libjulia-internal.so.1.13.3 | cut -d' ' -f1)" = 0af1866831d3f3b26b4cc18193db8730e47ba4c8019db1db4423982ce4606931
