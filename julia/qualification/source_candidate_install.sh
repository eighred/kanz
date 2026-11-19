#!/usr/bin/env bash
set -euo pipefail
cd /inputs
sha256sum --check --strict <<'CHECKSUMS'
5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4  julia.tar.gz
97a3b0d65c9e03a30bfdc89185ddf1ef0fe14b7e1a95d93d8ffc9cf57fb2a161  libcurl.tar.gz
16b92b2dd61998e52b58cd436dab506c7902bc2d0c590acba899a1df4e891d95  openssl.tar.gz
CHECKSUMS
tar -xzf julia.tar.gz --strip-components=1 --no-same-owner -C /opt/julia \
    --exclude='julia-1.13.3/share/julia/base/JuliaSyntax/docs' \
    --exclude='julia-1.13.3/share/julia/stdlib/v1.13/TOML/docs' \
    --exclude='julia-1.13.3/share/julia/stdlib/v1.13/TOML/benchmark' \
    --exclude='julia-1.13.3/share/julia/stdlib/v1.13/Tar/test' \
    --exclude='julia-1.13.3/share/julia/test'
cd /opt/julia
sha256sum --check --strict <<'CHECKSUMS'
443c0345cb2a10e55d41c385ef6dbf8af9209b82a2aaaeccf9772c04e398cfae  lib/julia/libcrypto.so.3
81fd035bfa9f36291cab3c3cfe172dc082a3ba439a3f25ebd125003d66c15602  lib/julia/libssl.so.3
a184796bc4b04a91ec47938d10ded0727b5d6d21fb96c3942ce32eb45faec8a2  lib/julia/libcurl.so.4.8.0
CHECKSUMS
mkdir -p share/licenses/kanz-openssl-runtime share/licenses/kanz-libcurl-runtime
for name in libcrypto.so.3 libssl.so.3; do
    test -f "lib/julia/$name" && test ! -L "lib/julia/$name"
    tar -xOf /inputs/openssl.tar.gz "$name" > "lib/julia/$name"
done
test -f lib/julia/libcurl.so.4.8.0 && test ! -L lib/julia/libcurl.so.4.8.0
tar -xOf /inputs/openssl.tar.gz LICENSE.txt > share/licenses/kanz-openssl-runtime/LICENSE.txt
tar -xOf /inputs/libcurl.tar.gz lib/libcurl.so.4.8.0 > lib/julia/libcurl.so.4.8.0
tar -xOf /inputs/libcurl.tar.gz share/licenses/LibCURL/COPYING > share/licenses/kanz-libcurl-runtime/COPYING
cd /opt/kanz-jl
cmp Manifest.toml /inputs/packages/Manifest.toml
python3 -B /tmp/offline_depot.py --input /inputs/packages --depot /opt/julia-depot
julia=(/opt/julia/bin/julia --startup-file=no --history-file=no --project=/opt/kanz-jl)
"${julia[@]}" -e 'using ProtoBuf'
"${julia[@]}" test/offline_depot.jl
"${julia[@]}" generate.jl
