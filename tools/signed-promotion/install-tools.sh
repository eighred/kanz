#!/usr/bin/env bash
set -euo pipefail
test "$(uname -s)" = Linux
test "$(uname -m)" = x86_64
test -n "${RUNNER_TEMP:-}"
install_dir="$RUNNER_TEMP/kanz-promotion-bin"
mkdir -p "$install_dir"
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 20 --max-time 120 \
  https://github.com/oras-project/oras/releases/download/v1.3.0/oras_1.3.0_linux_amd64.tar.gz \
  -o "$install_dir/oras.tgz"
echo "6cdc692f929100feb08aa8de584d02f7bcc30ec7d88bc2adc2054d782db57c64  $install_dir/oras.tgz" | sha256sum --check --strict
tar -xzf "$install_dir/oras.tgz" -C "$install_dir" oras
if [[ "${1:-}" == fixture ]]; then
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 20 --max-time 120 \
    https://github.com/distribution/distribution/releases/download/v3.0.0/registry_3.0.0_linux_amd64.tar.gz \
    -o "$install_dir/registry.tgz"
  echo "61c9a2c0d5981a78482025b6b69728521fbc78506d68b223d4a2eb825de5ca3d  $install_dir/registry.tgz" | sha256sum --check --strict
  tar -xzf "$install_dir/registry.tgz" -C "$install_dir" registry
else
  test "$#" -eq 0
fi
echo "$install_dir" >> "$GITHUB_PATH"
