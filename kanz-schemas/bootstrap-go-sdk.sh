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
net="$(awk '$1 == "golang.org/x/net" {print $2; exit}' "$consumer")"
if [[ -z "$goversion" || -z "$grpc" || -z "$protobuf" || -z "$net" ]]; then
  echo 'missing reviewed Go/gRPC/protobuf/x/net version in kanz/go.mod' >&2
  exit 1
fi
cd "$root/kanz-schemas/gen/go"
# Replace only the ignored generated manifest, including stale indirect pins
# left by an earlier generation. No tracked consumer dependencies are changed.
# gRPC's minimum x/net can lag the consumer's security patch. Carry that
# reviewed runtime pin into this independently scanned/generated module too.
printf 'module github.com/eighred/kanz/kanz-schemas-go\n\ngo %s\n\nrequire (\n google.golang.org/grpc %s\n google.golang.org/protobuf %s\n golang.org/x/net %s // indirect\n)\n' \
  "$goversion" "$grpc" "$protobuf" "$net" > go.mod
go mod tidy
