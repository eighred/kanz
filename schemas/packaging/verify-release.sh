#!/usr/bin/env bash
# The caller authenticates the release and signer before passing its downloaded files.
set -euo pipefail
if [[ $# != 3 ]]; then echo "usage: $0 X.Y.Z SOURCE_SHA ASSET_DIRECTORY" >&2; exit 2; fi
version=$1
source_sha=$2
assets=$(realpath "$3")
root=$(git rev-parse --show-toplevel)
cd "$root"
[[ "$(git rev-parse HEAD)" == "$source_sha" ]]
git diff --quiet HEAD
python3 schemas/packaging/manifest.py check "$version" "$source_sha" "$assets"

scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
fixtures="$scratch/kanz/test/contract/serialization/fixtures"
module=github.com/eighred/kanz/kanz-schemas-go
tag="v$version"
export GOWORK=off GOTOOLCHAIN=local
(
  cd kanz
  # Acquire only the public graph. An absent SDK can never trigger an external lookup.
  public_cache=$(go env GOMODCACHE)
  cp go.mod "$scratch/public.mod"
  cp go.sum "$scratch/public.sum"
  export GOFLAGS="-modfile=$scratch/public.mod"
  go mod edit -dropreplace="$module" -droprequire="$module"
  GOPRIVATE='github.com/eighred/*' GOVCS='*:off' go mod download

  cp go.mod "$scratch/consumer.mod"
  cp go.sum "$scratch/consumer.sum"
  export GOFLAGS="-modfile=$scratch/consumer.mod" GOMODCACHE="$scratch/empty-modules"
  export GOPROXY="file://$scratch/proxy" GONOPROXY=none GONOSUMDB="$module"
  python3 -m zipfile -e "$assets/kanz-schemas-go-proxy-$tag.zip" "$scratch/proxy"
  go mod edit -dropreplace="$module" -require="$module@$tag"
  go mod download -json "$module@$tag" > "$scratch/go-download.json"
  python3 - "$assets/sdk-manifest.json" "$scratch/go-download.json" <<'PY'
import json, sys
from pathlib import Path
manifest, observed = (json.loads(Path(path).read_bytes()) for path in sys.argv[1:])
assert manifest["go_module"] == {k: observed[k] for k in ("Path", "Version", "Sum", "GoModSum")}
PY
  export GOPROXY="file://$public_cache/cache/download"
  go test -mod=readonly -p 1 ./test/contract/serialization -run '^TestRoundTripParity$' -count=1 -v
  go run -mod=readonly ./test/contract/serialization/cmd/genfixtures -out "$fixtures"
  go mod verify
)

python3 -m venv "$scratch/runtime"
"$scratch/runtime/bin/pip" install --quiet --require-hashes --only-binary=:all: -r python/requirements.txt
"$scratch/runtime/bin/pip" install --no-index --no-deps "$assets/kanz_schemas-$version-py3-none-any.whl"
"$scratch/runtime/bin/pip" check
PYTHONPATH="$root/python" "$scratch/runtime/bin/python" - "$version" "$fixtures" <<'PY'
import json, sys
from importlib.metadata import version
from pathlib import Path
from inference.v1.inference_service_pb2_grpc import InferenceServiceStub
from kanz_bus import unframe, validate

assert version("kanz-schemas") == sys.argv[1]
directory = Path(sys.argv[2])
cases = json.loads((directory / "manifest.json").read_bytes())["fixtures"]
assert cases
for case in cases:
    envelope, payload = unframe((directory / case["file"]).read_bytes())
    validate(envelope)
    assert payload.hex() == case["payload_hex"] and envelope.event_id == case["envelope"]["event_id"]
print(f"Python consumed {len(cases)} Go serialization fixtures")
PY

consumer="$scratch/kanz/ts/kanz_bus"
mkdir -p "$consumer"
cp -R kanz/ts/kanz_bus/{package.json,tsconfig.json,src,test} "$consumer/"
cp schemas/packaging/ts/public-consumer-lock.json "$consumer/package-lock.json"
(
  cd "$consumer"
  npm pkg delete dependencies.@eighred/kanz-schemas
  npm ci --ignore-scripts --no-audit --no-fund
  npm pkg set "dependencies.@eighred/kanz-schemas=file:$assets/eighred-kanz-schemas-$version.tgz"
  npm install --offline --ignore-scripts --no-audit --no-fund
  npm run build
  npm test
)
