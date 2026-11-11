#!/usr/bin/env bash
# Fixed native overlay for the OCI image and the disposable Linux CI runtime.
set -euo pipefail
[[ $(uname -s) == Linux && $# == 4 ]] || exit 1
root=$1
julia_archive=$2
openssl_archive=$3
curl_archive=$4
[[ $root =~ ^/[A-Za-z0-9_./-]+$ && $(realpath -e -- "$root") == "$root" ]] || exit 1
for archive in "$julia_archive" "$openssl_archive" "$curl_archive"; do
  [[ $archive =~ ^/[A-Za-z0-9_./-]+$ && -f $archive && ! -L $archive && $(realpath -e -- "$archive") == "$archive" ]] || exit 1
done
# Neither the install root nor any parent may redirect writes through symlinks.
ancestor=$root
while [[ $ancestor != / ]]; do
  [[ -d $ancestor && ! -L $ancestor ]] || exit 1
  ancestor=$(dirname -- "$ancestor")
done
for path in "$root/lib" "$root/lib/julia" "$root/bin" "$root/share"; do
  [[ -d $path && ! -L $path ]] || exit 1
done
check() { printf '%s  %s\n' "$1" "$2" | sha256sum --check --strict --status; }
check 5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4 "$julia_archive"
check 8e2c75378726017fc66fb7fbbd10fb53522417c3e45a88af77b8853d46f2b063 "$openssl_archive"
check 97a3b0d65c9e03a30bfdc89185ddf1ef0fe14b7e1a95d93d8ffc9cf57fb2a161 "$curl_archive"
[[ -f $root/bin/julia && ! -L $root/bin/julia ]] || exit 1
check 6cbeea7d1416fb31952325b75f88c83173d939263767adc43e7b43c6de48ab03 "$root/bin/julia"
names=(libcrypto.so.3 libssl.so.3 libcurl.so.4.8.0)
original=(443c0345cb2a10e55d41c385ef6dbf8af9209b82a2aaaeccf9772c04e398cfae 81fd035bfa9f36291cab3c3cfe172dc082a3ba439a3f25ebd125003d66c15602 a184796bc4b04a91ec47938d10ded0727b5d6d21fb96c3942ce32eb45faec8a2)
replacement=(09ac6b8640f13b13badf5d2430a8cd326d27ad77668868ad6c9a36de87c4857d 044e0615a9f42e6e02298eec073325f528bee55c56901134788220fc5e96a098 1d977428a3d57ca07b091b2b8e18881119cb5822162fd345f24e94dae16e5fc2)
archives=("$openssl_archive" "$openssl_archive" "$curl_archive")
temporary=()
cleanup() { for file in "${temporary[@]}"; do [[ ! -e $file ]] || rm -- "$file"; done; }
trap cleanup EXIT
# Validate all original targets before preparing or replacing any native bytes.
for i in 0 1 2; do
  path=$root/lib/julia/${names[$i]}
  [[ -f $path && ! -L $path ]] || exit 1
  check "${original[$i]}" "$path"
done
for i in 0 1 2; do
  file=$(mktemp "$root/lib/julia/.kanz-native-XXXXXXXX")
  temporary+=("$file")
  tar -xOf "${archives[$i]}" "lib/${names[$i]}" > "$file"
  check "${replacement[$i]}" "$file"
  chmod 755 "$file"
done
# Licenses are installed under fresh fixed paths, never into linked directories.
[[ ! -L $root/share/licenses ]] || exit 1
mkdir -p -- "$root/share/licenses"
for directory in kanz-openssl-runtime kanz-libcurl-runtime; do
  [[ ! -e $root/share/licenses/$directory && ! -L $root/share/licenses/$directory ]] || exit 1
  mkdir -- "$root/share/licenses/$directory"
done
tar -xOf "$openssl_archive" share/licenses/OpenSSL/LICENSE.txt > "$root/share/licenses/kanz-openssl-runtime/LICENSE.txt"
tar -xOf "$curl_archive" share/licenses/LibCURL/COPYING > "$root/share/licenses/kanz-libcurl-runtime/COPYING"
check 7d5450cb2d142651b8afa315b5f238efc805dad827d91ba367d8516bc9d49e7a "$root/share/licenses/kanz-openssl-runtime/LICENSE.txt"
check 82f2f4427d6545ee5aaac4f0b80428da6cc8ba41c2cf5da3a03680ec327b9681 "$root/share/licenses/kanz-libcurl-runtime/COPYING"
for i in 0 1 2; do
  mv -T -- "${temporary[$i]}" "$root/lib/julia/${names[$i]}"
  check "${replacement[$i]}" "$root/lib/julia/${names[$i]}"
done
