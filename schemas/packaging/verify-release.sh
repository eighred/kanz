#!/usr/bin/env bash
# The caller authenticates the release and signer before passing its downloaded files.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
published=false
if [[ $# == 2 && $1 == --published ]]; then
  published=true
  download=$(realpath "$2")
  assets="$scratch/assets"
  mkdir -m 700 "$assets"
  # Use exactly the private snapshot whose contents are verified below.
  for filename in sdk-manifest.json kanz-schemas-go-proxy-v0.1.0.zip kanz_schemas-0.1.0-py3-none-any.whl eighred-kanz-schemas-0.1.0.tgz; do
    [[ -f "$download/$filename" && ! -L "$download/$filename" ]] || { echo "Missing regular asset $filename." >&2; exit 1; }
    cp -- "$download/$filename" "$assets/$filename"
  done
  cd "$root"
  version=0.1.0
  source_sha=f9b6941b70a48c8d243ab40b4c98c5f79f5ce0cf
  # Pinned to the immutable release attestation, independent of the download directory.
  (cd "$assets" && echo '4355e903c9feb94ed04fc37e78f8735f6b55666f1d29bcc821bf7cf32a8a8041  sdk-manifest.json' | sha256sum --check --strict)
  if [[ $(git rev-parse HEAD:schemas/proto) != a65e8edeb49122496b8ed0f00db35a1442e99d66 ]] ||
      [[ -n $(git status --porcelain --untracked-files=all -- schemas/proto) ]]; then
    echo 'Consumer schema sources differ from SDK v0.1.0.' >&2; exit 1
  fi
  python3 schemas/packaging/manifest.py assets "$version" "$source_sha" "$assets"
  : "${PIP_FIND_LINKS:?point PIP_FIND_LINKS at the prepared local Python wheel directory}"
  [[ -d "$PIP_FIND_LINKS" ]] || { echo 'Expected a local wheel directory.' >&2; exit 1; }
  export PIP_NO_INDEX=1 PIP_CONFIG_FILE=/dev/null PIP_DISABLE_PIP_VERSION_CHECK=1
  export npm_config_offline=true GOVCS='*:off'
else
  if [[ $# != 3 ]]; then
    echo "usage: $0 X.Y.Z SOURCE_SHA ASSET_DIRECTORY | --published ASSET_DIRECTORY" >&2; exit 2
  fi
  version=$1
  source_sha=$2
  assets=$(realpath "$3")
  cd "$root"
  [[ "$(git rev-parse HEAD)" == "$source_sha" ]]
  git diff --quiet HEAD
  python3 schemas/packaging/manifest.py check "$version" "$source_sha" "$assets"
fi

fixtures="$scratch/kanz/test/contract/serialization/fixtures"
module=github.com/eighred/kanz/kanz-schemas-go
tag="v$version"
export GOWORK=off GOTOOLCHAIN=local
(
  cd kanz
  # Acquire only the public graph. An absent SDK can never trigger an external lookup.
  public_cache=$(go env GOMODCACHE)
  if "$published"; then
    # Keep the public checksum verifier and restrict its lookup to the local cache.
    export GOSUMDB="sum.golang.org file://$public_cache/cache/download/sumdb/sum.golang.org"
  else
    cp go.mod "$scratch/public.mod"
    cp go.sum "$scratch/public.sum"
    export GOFLAGS="-modfile=$scratch/public.mod"
    go mod edit -dropreplace="$module" -droprequire="$module"
    GOPRIVATE='github.com/eighred/*' GOVCS='*:off' go mod download
  fi

  cp go.mod "$scratch/consumer.mod"
  cp go.sum "$scratch/consumer.sum"
  export GOFLAGS="-modfile=$scratch/consumer.mod -modcacherw" GOMODCACHE="$scratch/empty-modules"
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
  if "$published"; then
    go build -mod=readonly -o "$scratch/bin/" ./services/accounting/cmd/accounting ./services/api_gateway/cmd/api_gateway ./services/web_bff/cmd/web_bff
    go test -mod=readonly -p 1 -count=1 ./services/accounting/internal/server -run '^TestBreakQueue(IsServedWithAges|RefusesAnotherTenant)$' -v
  fi
  go mod verify
)

python3 -m venv "$scratch/runtime"
"$scratch/runtime/bin/pip" install --quiet --require-hashes --only-binary=:all: -r python/requirements.txt
"$scratch/runtime/bin/pip" install --no-index --no-deps "$assets/kanz_schemas-$version-py3-none-any.whl"
"$scratch/runtime/bin/pip" check
if "$published"; then
  "$scratch/runtime/bin/pip" install --quiet --only-binary=:all: pytest pytest-asyncio
  PYTHONPATH="$root/python" "$scratch/runtime/bin/python" -m pytest -q -p no:cacheprovider \
    python/tests/test_inference_service.py python/tests/test_interactive_servicer.py
fi
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
  KANZ_REQUIRE_SERIALIZATION_FIXTURES=1 npm test
)
