#!/usr/bin/env bash
set -Eeuo pipefail

: "${CI_JOB_TOKEN:?CI_JOB_TOKEN secret is required}"
: "${TAG_NAME:?TAG_NAME is required}"
: "${RELEASE_TITLE:?RELEASE_TITLE is required}"
: "${RELEASE_BODY_FILE:?RELEASE_BODY_FILE is required}"

OWNER="${GITVERSE_OWNER:-kashtan404}"
REPO="${GITVERSE_REPO:-nclite}"
API_BASE="${GITVERSE_API_BASE:-https://api.gitverse.ru}"
ASSET_DIR="${ASSET_DIR:-release-assets}"
API_ROOT="${API_BASE}/repos/${OWNER}/${REPO}"
ACCEPT='application/vnd.gitverse.object+json;version=1'

for cmd in curl jq; do
  command -v "$cmd" >/dev/null 2>&1 || { echo "Missing command: $cmd" >&2; exit 2; }
done
[[ -s "$RELEASE_BODY_FILE" ]] || { echo "Missing release notes: $RELEASE_BODY_FILE" >&2; exit 2; }

shopt -s nullglob
assets=(
  "$ASSET_DIR"/*-source.tar.gz
  "$ASSET_DIR"/*-linux-arm64.bin
  "$ASSET_DIR"/CHANGELOG-*.md
  "$ASSET_DIR"/SHA256SUMS-*.txt
)
[[ ${#assets[@]} -eq 4 ]] || {
  echo "Expected 4 assets, found ${#assets[@]}" >&2
  printf '  %s\n' "${assets[@]}" >&2
  exit 2
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
headers=(-H "Authorization: Bearer ${CI_JOB_TOKEN}" -H "Accept: ${ACCEPT}")

api_request() {
  local output="$1"; shift
  local code rc
  set +e
  code="$(curl --silent --show-error --connect-timeout 30 --max-time 1800 \
    "${headers[@]}" "$@" -o "$output" -w '%{http_code}')"
  rc=$?
  set -e
  if [[ $rc -ne 0 ]]; then
    echo "curl failed with exit code $rc" >&2
    [[ -f "$output" ]] && cat "$output" >&2
    return "$rc"
  fi
  printf '%s' "$code"
}

tag_encoded="$(jq -rn --arg v "$TAG_NAME" '$v|@uri')"
get_code="$(api_request "$tmp/get.json" "${API_ROOT}/releases/tags/${tag_encoded}")"

case "$get_code" in
  200)
    release_id="$(jq -er '.id' "$tmp/get.json")"
    echo "Reusing release id=$release_id"
    ;;
  404)
    jq -n --arg tag "$TAG_NAME" --arg title "$RELEASE_TITLE" \
      --rawfile body "$RELEASE_BODY_FILE" \
      '{tag_name:$tag,name:$title,target_commitish:$tag,body:$body,
        draft:false,prerelease:false,is_authorized_only:false}' > "$tmp/create.json"
    code="$(api_request "$tmp/create-response.json" \
      -H 'Content-Type: application/json' -X POST \
      --data-binary "@$tmp/create.json" "${API_ROOT}/releases")"
    [[ "$code" == "201" ]] || {
      echo "Create release failed (HTTP $code):" >&2; cat "$tmp/create-response.json" >&2; exit 1;
    }
    release_id="$(jq -er '.id' "$tmp/create-response.json")"
    ;;
  *)
    echo "Release lookup failed (HTTP $get_code):" >&2
    cat "$tmp/get.json" >&2
    exit 1
    ;;
esac

jq -n --arg tag "$TAG_NAME" --arg title "$RELEASE_TITLE" \
  --rawfile body "$RELEASE_BODY_FILE" \
  '{tag_name:$tag,name:$title,target_commitish:$tag,body:$body,
    draft:false,prerelease:false,is_authorized_only:false}' > "$tmp/update.json"
code="$(api_request "$tmp/update-response.json" \
  -H 'Content-Type: application/json' -X PATCH \
  --data-binary "@$tmp/update.json" "${API_ROOT}/releases/${release_id}")"
[[ "$code" == "200" ]] || {
  echo "Update release failed (HTTP $code):" >&2; cat "$tmp/update-response.json" >&2; exit 1;
}

assets_url="${API_ROOT}/releases/${release_id}/assets"
for asset in "${assets[@]}"; do
  name="$(basename "$asset")"
  echo "Uploading $name ($(stat -c '%s' "$asset") bytes)"

  code="$(api_request "$tmp/assets.json" "${assets_url}?per_page=50&page=1")"
  [[ "$code" == "200" ]] || {
    echo "List assets failed (HTTP $code):" >&2; cat "$tmp/assets.json" >&2; exit 1;
  }

  existing_id="$(jq -r --arg name "$name" '.[]? | select(.name==$name) | .id' "$tmp/assets.json" | head -n1)"
  if [[ -n "$existing_id" && "$existing_id" != "null" ]]; then
    code="$(api_request "$tmp/delete.json" -X DELETE "${assets_url}/${existing_id}")"
    [[ "$code" == "200" || "$code" == "204" ]] || {
      echo "Delete old asset failed (HTTP $code):" >&2; cat "$tmp/delete.json" >&2; exit 1;
    }
  fi

  # curl adds the multipart boundary itself. Large source archives get up to 30 min.
  name_encoded="$(jq -rn --arg v "$name" '$v|@uri')"
  code="$(api_request "$tmp/upload.json" \
    -F "attachment=@${asset};filename=${name}" \
    "${assets_url}?name=${name_encoded}")"
  [[ "$code" == "201" ]] || {
    echo "Upload $name failed (HTTP $code):" >&2
    cat "$tmp/upload.json" >&2
    exit 1
  }
  jq -r '"Uploaded \(.name) (\(.size) bytes), id=\(.id)"' "$tmp/upload.json"
done

echo "Release published: https://gitverse.ru/${OWNER}/${REPO}/releases/tag/${TAG_NAME}"
