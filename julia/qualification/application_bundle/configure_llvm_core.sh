#!/bin/bash
set -eu
# The complete product configure continues into unselected backend source after it writes both headers.
test "$(sha256sum /zlib.tar.gz | cut -d' ' -f1)" = b9b82fcdbb893ca7470e326b9fa305ece5bf5f462004a622cfdbf54c5d8de4e5
test "$(sha256sum /zstd.tar.gz | cut -d' ' -f1)" = 8b2d56159d22748eca8137d4df63699be3e01e3d788bd36f92c65750f8168dc6
echo "0964601c2bfcac083e04a55d369408b62ff167dab42928dadd74d21ac47019be  /source.tar" | sha256sum -c -
mkdir /tmp/s /tmp/b /tmp/local
tar -xf /source.tar -C /tmp/s
chmod +x /tmp/s/llvm/cmake/config.guess
tar -xf /zlib.tar.gz -C /tmp/local
tar -xf /zstd.tar.gz -C /tmp/local
export CC=x86_64-linux-gnu-gcc CXX=x86_64-linux-gnu-g++ AR=x86_64-linux-gnu-ar RANLIB=x86_64-linux-gnu-ranlib
cmake -S /tmp/s/llvm -B /tmp/b -G Ninja \
  -DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR=x86_64 -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_C_COMPILER=x86_64-linux-gnu-gcc -DCMAKE_CXX_COMPILER=x86_64-linux-gnu-g++ \
  -DCMAKE_CXX_FLAGS=-fno-gnu-unique -DCMAKE_SHARED_LINKER_FLAGS=-Wl,--build-id \
  -DCMAKE_PREFIX_PATH=/tmp/local -DCMAKE_INSTALL_PREFIX=/workspace/destdir \
  -DLLVM_HOST_TRIPLE=x86_64-linux-gnu '-DLLVM_TARGETS_TO_BUILD=host;AMDGPU;NVPTX;WebAssembly;BPF;AVR' \
  '-DLLVM_ENABLE_PROJECTS=llvm;clang;clang-tools-extra;compiler-rt;lld;mlir' \
  -DLLVM_USE_PERF=1 -DLLVM_USE_INTEL_JITEVENTS=1 -DLLVM_ENABLE_ZLIB=FORCE_ON -DLLVM_ENABLE_ZSTD=FORCE_ON \
  -DLLVM_ENABLE_LIBXML2=OFF -DLLVM_ENABLE_TERMINFO=OFF -DHAVE_HISTEDIT_H=OFF -DHAVE_LIBEDIT=OFF \
  -DLLVM_BUILD_LLVM_DYLIB=ON -DLLVM_LINK_LLVM_DYLIB=ON -DLLVM_VERSION_SUFFIX=jl \
  -DLLVM_SHLIB_SYMBOL_VERSION=JL_LLVM_20.1 > /out/private-config.log 2>&1 || status=$?
cp /tmp/b/CMakeCache.txt /out/
cp /tmp/b/include/llvm/Config/config.h /out/config.h
cp /tmp/b/include/llvm/Config/llvm-config.h /out/llvm-config.h
echo "a711cbb07a11e383a5c7e281cec21872765a5561aae58751c2c1f9109a7cfebf  /out/config.h" | sha256sum -c -
echo "d8fb9d66b95412cca2306a8f5f4f05b6f9cbaf57812dbd7f555aa8947b1bfe15  /out/llvm-config.h" | sha256sum -c -
# Preserve the result of the complete configure independently of the two headers.
printf '%s\n' "${status:-0}" > /out/whole-project-configure.exit
exit "${status:-0}"
