#!/usr/bin/env bash
set -euo pipefail

# The SDK is generated, but its runtime dependencies must be the reviewed
# consumer graph. An empty module followed by tidy selects today's latest gRPC
# release and can make yesterday's unchanged commit fail -mod=readonly.
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
consumer="$root/kanz/go.mod"
goversion="$(awk '$1 == "go" {print $2; exit}' "$consumer")"
grpc="$(awk '$1 == "google.golang.org/grpc" {print $2; exit}' "$consumer")"
protobuf="$(awk '$1 == "google.golang.org/protobuf" {print $2; exit}' "$consumer")"
if [[ -z "$goversion" || -z "$grpc" || -z "$protobuf" ]]; then
  echo 'missing reviewed Go/gRPC/protobuf version in kanz/go.mod' >&2
  exit 1
fi
cd "$root/kanz-schemas/gen/go"
# Replace only the ignored generated manifest, including stale indirect pins
# left by an earlier generation. No tracked consumer dependencies are changed.
printf 'module github.com/eighred/kanz/kanz-schemas-go\n\ngo %s\n\nrequire (\n google.golang.org/grpc %s\n google.golang.org/protobuf %s\n)\n' \
  "$goversion" "$grpc" "$protobuf" > go.mod
go mod tidy
