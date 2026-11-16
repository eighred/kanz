#!/usr/bin/env bash
# Same reviewed scanner versions as release.yml; independent immutable archives.
set -euo pipefail
tools="$RUNNER_TEMP/julia-audit-tools"
mkdir -p "$tools"
curl --fail --location --retry 3 -o "$tools/trivy.tar.gz" https://github.com/aquasecurity/trivy/releases/download/v0.72.0/trivy_0.72.0_Linux-64bit.tar.gz
echo "bbb64b9695866ce4a7a8f5c9592002c5961cab378577fa3f8a040df362b9b2ea  $tools/trivy.tar.gz" | sha256sum --check --strict
curl --fail --location --retry 3 -o "$tools/syft.tar.gz" https://github.com/anchore/syft/releases/download/v1.56.0/syft_1.56.0_linux_amd64.tar.gz
echo "1c2493cfcf52d3367d6cb25780d9190c111c2219c34c820574c43e8bae15c528  $tools/syft.tar.gz" | sha256sum --check --strict
tar -xzf "$tools/trivy.tar.gz" -C "$tools" trivy
tar -xzf "$tools/syft.tar.gz" -C "$tools" syft
echo "$tools" >> "$GITHUB_PATH"
