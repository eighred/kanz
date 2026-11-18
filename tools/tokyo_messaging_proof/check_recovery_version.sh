#!/usr/bin/env bash
# Contract fixture: a second write makes key-only reads select other bytes.
set -euo pipefail
source "$(dirname "$0")/../testnet-data-plane-recovery.sh" backup fixture-bucket fixture-kms
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
archive="$work/bundle.tar.gz"
printf 'first backup\n' >"$archive"
fixture_sha=$(sha256sum "$archive" | cut -d' ' -f1)
key=testnet/data-plane/fixture/bundle.tar.gz
case_name=valid

aws() {
  local operation=$2 arg pinned=false version='first+version/='
  for arg in "$@"; do
    if [[ "$arg" == "--version-id=$version" ]]; then pinned=true; fi
  done
  case "$operation" in
    put-object)
      case "$case_name" in
        absent) echo '{}' ;;
        suspended) echo '{"VersionId":"null"}' ;;
        *) jq -n --arg version "$version" '{VersionId:$version}' ;;
      esac ;;
    head-object)
      if [[ "$pinned" != true || "$case_name" == changed_head ]]; then version=second; fi
      jq -n --arg id "$version" --arg sha "$fixture_sha" \
        '{VersionId:$id,Metadata:{sha256:$sha},ContentLength:13,ServerSideEncryption:"aws:kms",SSEKMSKeyId:"fixture-kms"}' ;;
    get-object-retention)
      [[ "$pinned" == true ]] || return 1
      local until=2100-01-01T00:00:00Z
      if [[ "$case_name" == expired ]]; then until=2000-01-01T00:00:00Z; fi
      jq -n --arg until "$until" '{Retention:{Mode:"GOVERNANCE",RetainUntilDate:$until}}' ;;
    get-object)
      if [[ "$pinned" != true || "$case_name" == changed_download ]]; then version=second; fi
      if [[ "$case_name" == corrupt ]]; then echo other >"${!#}"; else cp "$archive" "${!#}"; fi
      jq -n --arg id "$version" '{VersionId:$id}' ;;
    *) return 1 ;;
  esac
}

result=$(upload_recovery_archive "$key" "$archive" "$fixture_sha")
jq -e --arg sha "$fixture_sha" '.version_id=="first+version/=" and .sha256==$sha and
  .object_lock_retain_until=="2100-01-01T00:00:00Z"' <<<"$result" >/dev/null
for case_name in absent suspended changed_head expired changed_download corrupt; do
  if (upload_recovery_archive "$key" "$archive" "$fixture_sha") >"$work/result" 2>/dev/null; then
    echo "Accepted invalid archive in case $case_name" >&2; exit 1
  fi
  [[ ! -s "$work/result" ]]
done
echo 'Verified pinned S3 requests and six object custody refusals'
