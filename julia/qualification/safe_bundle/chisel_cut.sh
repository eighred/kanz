#!/usr/bin/env bash
# Run in a disposable Linux amd64 builder with bash, curl, file, dpkg-deb and GNU tar.
set -euo pipefail
out=$(realpath "${1:?pass an existing empty output directory}")
[[ -d "$out" && -z "$(ls -A "$out")" ]] || { echo "Expected an empty output directory" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
cd "$work"
fetch() {
    curl --fail --silent --show-error --location --proto '=https' "$2" -o "$1"
    echo "$3  $1" | sha256sum --check --status
}

fetch chisel.tgz https://github.com/canonical/chisel/releases/download/v1.5.1/chisel_v1.5.1_linux_amd64.tar.gz \
    caa1f84ef144c3311d95ee464c1b72dd237e99055a8c58b791a5591abd42d9a1
fetch wrapper https://raw.githubusercontent.com/canonical/rocks-toolbox/756bea37166f5ae224e5b3f6bfbfb00ad367c865/chisel-wrapper \
    6186db1e6a9cfb157e3a256a1eff26ba144096b02d4249eba9ac5d6681a646b9
fetch definitions.tgz https://codeload.github.com/canonical/chisel-releases/tar.gz/a84c5eb70f22c34212c6582f603467498061c978 \
    99f546c8c331d5f41d2d18992e4dedf7ce6077f77a925e28e84fa796ecd4796d
mkdir definitions control base
tar -xzf chisel.tgz chisel
tar -xzf definitions.tgz -C definitions --strip-components=1
export PATH="$work:$PATH" HOME="$work"
mkdir -p base/var/lib/dpkg
bash wrapper --generate-dpkg-status "$work/base/var/lib/dpkg/status" -- \
    --arch amd64 --release "$work/definitions" --root "$work/base" \
    base-files_chisel base-files_release-info base-files_tmp libc6_libs \
    ca-certificates_data tzdata_zoneinfo tzdata-legacy_zoneinfo
chisel cut --arch amd64 --release "$work/definitions" --root "$work/control" \
    base-files_chisel libc6_gconv
for rootfs in base control; do
    tar --sort=name --mtime=@0 --numeric-owner --owner=0 --group=0 \
        -cf "$out/$rootfs.tar" -C "$rootfs" .
done
