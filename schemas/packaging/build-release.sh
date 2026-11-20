#!/usr/bin/env bash
# Assemble and test all SDK assets in a clean Linux checkout. This has no release credentials.
set -euo pipefail
if [[ $# != 3 ]]; then echo "usage: $0 X.Y.Z SOURCE_SHA NEW_OUTPUT_DIRECTORY" >&2; exit 2; fi
version=$1
source_sha=$2
output=$(realpath -m "$3")
root=$(git rev-parse --show-toplevel)
cd "$root"
[[ "$source_sha" =~ ^[0-9a-f]{40}$ && "$(git rev-parse HEAD)" == "$source_sha" ]]
[[ "$version" =~ ^(0|1)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ -z "$(git status --porcelain --untracked-files=all)" && ! -e schemas/gen ]]
[[ ! -e "$output" && "$output" != "$root" && "$output" != "$root/"* ]]
[[ "$(go env GOVERSION)" == go1.27.4 && "$(buf --version)" == 1.69.0 ]]
[[ "$(node --version)" == v24.22.2 && "$(python3 --version)" == "Python 3.12.16" ]]

scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
mkdir "$scratch/bin" "$scratch/assets"
export GOBIN="$scratch/bin"
export PATH="$GOBIN:$PATH"
export SOURCE_DATE_EPOCH
SOURCE_DATE_EPOCH=$(git show -s --format=%ct "$source_sha")
make -C kanz bootstrap generate

# The inference image uses the hash-locked grpc_tools generator. This is the exact
# producer paired with its checked runtime. The Buf-generated Python directory is distinct.
python3 -m venv "$scratch/codegen"
"$scratch/codegen/bin/pip" install --quiet --require-hashes --only-binary=:all: -r python/requirements-codegen.txt
mkdir "$scratch/python"
mapfile -d '' -t protos < <(find schemas/proto -name '*.proto' -print0 | sort -z)
"$scratch/codegen/bin/python" -m grpc_tools.protoc -I schemas/proto --descriptor_set_out="$scratch/schemas.pb" --python_out="$scratch/python" \
  --pyi_out="$scratch/python" --grpc_python_out="$scratch/python" "${protos[@]}"
"$scratch/codegen/bin/python" schemas/packaging/assemble.py go --version "$version" --generated schemas/gen/go \
  --descriptors "$scratch/schemas.pb" --output "$scratch/assets"
"$scratch/codegen/bin/python" schemas/packaging/assemble.py python --version "$version" \
  --generated "$scratch/python" --output "$scratch/assets"
cp schemas/packaging/ts/package.json schemas/gen/ts/package.json
(
  cd schemas/gen/ts
  npm pkg set "version=$version"
  npm pack --ignore-scripts --pack-destination "$scratch/assets"
)

python3 schemas/packaging/manifest.py write "$version" "$source_sha" "$scratch/assets"
bash schemas/packaging/verify-release.sh "$version" "$source_sha" "$scratch/assets"
mkdir "$output"
cp "$scratch/assets/"* "$output/"
