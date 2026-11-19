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

# Even the older key now has a newer version. Only the retained receipt
# identifies the original checkpoint. Default discovery should select today.
(
  old_key='testnet/data-plane/20261118T010000Z-123456789abc/bundle.tar.gz'
  new_key='testnet/data-plane/20261119T010000Z-fedcba987654/bundle.tar.gz'
  old_version="old+/=v'1"
  listing=$(jq -cn --arg new "$new_key" --arg old "$old_key" \
    '{Contents:[{Key:$new,LastModified:"2026-11-19T01:00:00Z"},{Key:$old,LastModified:"2026-11-18T01:00:00Z"}]}')
  aws() {
    local op=$2 key='' version='' bytes sha
    shift 2
    while (($#)); do
      case $1 in
        --key) key=$2; shift ;;
        --version-id=*) version=${1#*=} ;;
      esac
      if [[ $# == 1 && "$op" == get-object ]]; then local output=$1; fi
      shift
    done
    printf '%s|%s|%s\n' "$op" "$key" "$version" >>"$work/history-calls"
    if [[ "$op" == list-objects-v2 ]]; then printf '%s\n' "$listing"; return; fi
    case "$key" in
      "$old_key") version=${version:-overwritten} ;;
      "$new_key") version=${version:-latest} ;;
      *) return 1 ;;
    esac
    case "$key:$version" in
      "$old_key:$old_version") bytes=prior-checkpoint ;;
      "$old_key:overwritten") bytes=replaced-checkpoint ;;
      "$new_key:latest") bytes=latest-checkpoint ;;
      *) return 1 ;;
    esac
    sha=$(printf '%s' "$bytes" | sha256sum | cut -d' ' -f1)
    case "$op" in
      head-object) jq -n --arg v "$version" --arg s "$sha" --argjson len "${#bytes}" \
        '{VersionId:$v,Metadata:{sha256:$s},ContentLength:$len,ServerSideEncryption:"aws:kms",SSEKMSKeyId:"fixture-kms"}' ;;
      get-object-retention) echo '{"Retention":{"Mode":"GOVERNANCE","RetainUntilDate":"2100-01-01T00:00:00Z"}}' ;;
      get-object) printf '%s' "$bytes" >"$output"; jq -n --arg v "$version" '{VersionId:$v}' ;;
      *) return 1 ;;
    esac
  }
  old_sha=$(printf '%s' prior-checkpoint | sha256sum | cut -d' ' -f1)
  selection=$(jq -cn --arg key "$old_key" --arg version "$old_version" --arg sha "$old_sha" \
    '{key:$key,version_id:$version,sha256:$sha}')
  result=$(download_restore_archive "$work/selected" "$(printf '%s' "$selection" | base64 -w0)")
  jq -e --argjson want "$selection" 'contains($want)' <<<"$result" >/dev/null
  [[ "$(cat "$work/selected")" == prior-checkpoint ]]
  [[ "$(cat "$work/history-calls")" == "$(printf '%s|%s|%s\n' head-object "$old_key" "$old_version" \
    get-object-retention "$old_key" "$old_version" get-object "$old_key" "$old_version")" ]]
  result=$(download_restore_archive "$work/latest")
  jq -e --arg k "$new_key" '.key==$k and .version_id=="latest"' <<<"$result" >/dev/null
  [[ "$(cat "$work/latest")" == latest-checkpoint ]]

  valid_listing=$listing
  for filter in '.Contents=[]' '.IsTruncated=true' '.NextToken="pending"' \
      'del(.Contents[1].LastModified)' 'del(.Contents[1].Key)' '.Contents[0].Key="testnet/data-plane/notes"'; do
    listing=$(jq -c "$filter" <<<"$valid_listing")
    : >"$work/history-calls"
    if (download_restore_archive "$work/refused-latest") >"$work/result" 2>/dev/null; then exit 1; fi
    [[ "$(cat "$work/history-calls")" == 'list-objects-v2||' && ! -s "$work/result" && ! -e "$work/refused-latest" ]]
  done
  listing=$valid_listing

  old_version=$(jq -nr '("\u00e9"*512)')
  boundary=$(jq -c --arg v "$old_version" '.version_id=$v' <<<"$selection")
  result=$(download_restore_archive "$work/boundary" "$(printf '%s' "$boundary" | base64 -w0)")
  jq -e --arg v "$old_version" '.version_id==$v and (.version_id|utf8bytelength)==1024' <<<"$result" >/dev/null
  : >"$work/history-calls"
  selected=$(jq -c '.version_id+="a"' <<<"$boundary" | base64 -w0)
  if (download_restore_archive "$work/rejected" "$selected") >"$work/result" 2>/dev/null; then exit 1; fi
  [[ ! -s "$work/history-calls" && ! -s "$work/result" && ! -e "$work/rejected" ]]
  old_version="old+/=v'1"

  for filter in '.sha256=("f"*64)' '.version_id="missing"' '.key="outside"' 'del(.sha256)' '.version_id="null"'; do
    selected=$(jq -c "$filter" <<<"$selection" | base64 -w0)
    : >"$work/history-calls"
    if (download_restore_archive "$work/rejected" "$selected") >"$work/result" 2>/dev/null; then exit 1; fi
    [[ ! -s "$work/result" && ! -e "$work/rejected" ]]
    if grep -q '^list-objects-v2|' "$work/history-calls"; then echo 'Invalid receipt fell back to latest' >&2; exit 1; fi
  done
  for field in key sha256; do
    selected=$(jq -c --arg field "$field" '.[$field] |= ("\n" + .)' <<<"$selection" | base64 -w0)
    : >"$work/history-calls"
    if (download_restore_archive "$work/rejected" "$selected") >"$work/result" 2>/dev/null; then exit 1; fi
    [[ ! -s "$work/result" && ! -s "$work/history-calls" && ! -e "$work/rejected" ]]
  done
  echo 'Verified retained version selection, complete latest discovery, six listing refusals and eight receipt refusals'
)

# Inspect the production backup paths at its first post-allocation API call.
# A refused lookup must also remove the private directory.
path_record="$work/observed-directory"
require_boundary() { :; }
refuse_active_writers() { :; }
k() {
  [[ "$*" == *"get cluster kanz-testnet-postgres"* ]] || return 1
  [[ "$(stat -c %a "$RECOVERY_WORK")" == 700 && "$work" == "$RECOVERY_WORK/payload" &&
     "$archive" == "$RECOVERY_WORK/bundle.tar.gz" ]] || return 1
  echo "$RECOVERY_WORK" >"$path_record"
  return 1
}
if (backup) >/dev/null 2>&1; then exit 1; fi
[[ -s "$path_record" && ! -e "$(cat "$path_record")" ]]
