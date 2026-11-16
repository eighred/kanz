#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$RUNNER_TEMP/feed-tools"
curl -fsSL https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64 -o "$RUNNER_TEMP/feed-tools/kind"
echo "aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d  $RUNNER_TEMP/feed-tools/kind" | sha256sum -c
chmod +x "$RUNNER_TEMP/feed-tools/kind"
curl -fsSL https://get.helm.sh/helm-v4.3.1-linux-amd64.tar.gz -o "$RUNNER_TEMP/helm.tar.gz"
echo "0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b  $RUNNER_TEMP/helm.tar.gz" | sha256sum -c
tar -xzf "$RUNNER_TEMP/helm.tar.gz" -C "$RUNNER_TEMP/feed-tools" linux-amd64/helm
mv "$RUNNER_TEMP/feed-tools/linux-amd64/helm" "$RUNNER_TEMP/feed-tools/helm"
echo "$RUNNER_TEMP/feed-tools" >> "$GITHUB_PATH"
curl -fsSL https://helm.cilium.io/cilium-1.20.4.tgz -o "$RUNNER_TEMP/cilium.tgz"
echo "505abff8385811df1a949776fc42e9f10697989a332acc86d6e8933efcb855ed  $RUNNER_TEMP/cilium.tgz" | sha256sum -c

curl -fsSL https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubectl -o "$RUNNER_TEMP/feed-tools/kubectl"
echo "8b8f088da2dab964f853b38464033b1be15ede2839eca751482357c45abdd05a  $RUNNER_TEMP/feed-tools/kubectl" | sha256sum -c
chmod +x "$RUNNER_TEMP/feed-tools/kubectl"
