#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly INDEX_FILENAME="taugrid-release-sbom-index.json"
readonly SCHEMA_VERSION="2.0"

fail() {
  echo "release SBOM error: $*" >&2
  exit 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

validate_spdx() {
  local path="$1"
  local expected_name="$2"
  local expected_namespace="$3"
  jq -e \
    --arg name "$expected_name" \
    --arg namespace "$expected_namespace" '
      .SPDXID == "SPDXRef-DOCUMENT"
      and (.spdxVersion | startswith("SPDX-2."))
      and .dataLicense == "CC0-1.0"
      and .name == $name
      and .documentNamespace == $namespace
      and (.creationInfo.created | type == "string")
      and (.creationInfo.creators | length > 0)
      and (((.packages // []) | length > 0) or ((.files // []) | length > 0))
      and (
        ((.documentDescribes // []) | length > 0)
        or any(
          (.relationships // [])[];
          .spdxElementId == "SPDXRef-DOCUMENT"
          and .relationshipType == "DESCRIBES"
        )
      )
    ' "$path" >/dev/null ||
    fail "$path is not the expected SPDX document"
}

expected_assets() {
  local release_dir="$1"
  local wheel
  local wheel_count
  wheel_count="$(
    find "$release_dir" -maxdepth 1 -type f -name 'tau-*.whl' | wc -l | tr -d ' '
  )"
  (( wheel_count == 1 )) || fail "release must contain exactly one tau-*.whl asset"
  wheel="$(
    find "$release_dir" -maxdepth 1 -type f -name 'tau-*.whl' -exec basename {} \;
  )"
  {
    printf '%s\n' \
      LICENSE \
      SHA256SUMS \
      install.sh \
      install.ps1 \
      tau-darwin-amd64 \
      tau-darwin-arm64 \
      tau-gen-darwin-amd64 \
      tau-gen-darwin-arm64 \
      tau-gen-linux-amd64 \
      tau-gen-linux-arm64 \
      tau-linux-amd64 \
      tau-linux-arm64 \
      tau-windows-amd64.exe \
      tau-gen-windows-amd64.exe \
      "$wheel" \
      "$INDEX_FILENAME"
    jq -r '.cli[].sbom, .images[].sbom' "$release_dir/$INDEX_FILENAME"
  } | sort
}

validate_index() {
  local release_dir="$1"
  local index="$release_dir/$INDEX_FILENAME"
  test -f "$index" || fail "release is missing $INDEX_FILENAME"

  jq -e \
    --arg schema "$SCHEMA_VERSION" '
      .schemaVersion == $schema
      and (.release.tag | test("^v[0-9]+\\.[0-9]+\\.[0-9]+([+-][0-9A-Za-z.-]+)?$"))
      and (.release.sourceCommit | test("^[0-9a-f]{40}$"))
      and (.release.generatedAt | type == "string")
      and (.cli | length == 10)
      and (.images | length == 6)
      and ([.cli[] | [.component, .target, .asset]] | unique | length == 10)
      and ([.images[] | [.component, .platform]] | unique | length == 6)
      and ([.images[].component] | unique | sort == ["tau", "tau-core-controller", "taugrid-portal"])
      and ([.images[].platform] | unique | sort == ["linux/amd64", "linux/arm64"])
    ' "$index" >/dev/null || fail "$INDEX_FILENAME has invalid release coverage"

  while IFS=$'\t' read -r component target version asset asset_sha sbom sbom_sha; do
    local expected_sbom="$asset.spdx.json"
    local namespace="https://github.com/Azure/taugrid/sbom/${version}/${expected_sbom}"
    test "$sbom" = "$expected_sbom" || fail "unexpected CLI SBOM filename for $asset"
    test -f "$release_dir/$asset" || fail "missing release binary $asset"
    test -f "$release_dir/$sbom" || fail "missing CLI SBOM $sbom"
    test "$(sha256_file "$release_dir/$asset")" = "$asset_sha" ||
      fail "CLI asset digest mismatch for $asset"
    test "$(sha256_file "$release_dir/$sbom")" = "$sbom_sha" ||
      fail "CLI SBOM digest mismatch for $asset"
    validate_spdx "$release_dir/$sbom" "$asset" "$namespace"
    case "$component:$target:$asset" in
      tau:darwin-amd64:tau-darwin-amd64 | \
        tau:darwin-arm64:tau-darwin-arm64 | \
        tau:linux-amd64:tau-linux-amd64 | \
        tau:linux-arm64:tau-linux-arm64 | \
        tau:windows-amd64:tau-windows-amd64.exe | \
        tau-gen:darwin-amd64:tau-gen-darwin-amd64 | \
        tau-gen:darwin-arm64:tau-gen-darwin-arm64 | \
        tau-gen:linux-amd64:tau-gen-linux-amd64 | \
        tau-gen:linux-arm64:tau-gen-linux-arm64 | \
        tau-gen:windows-amd64:tau-gen-windows-amd64.exe) ;;
      *) fail "unexpected CLI release entry $component:$target:$asset" ;;
    esac
  done < <(
    jq -r '.cli[] | [
      .component, .target, .version, .asset, .assetSha256, .sbom, .sbomSha256
    ] | @tsv' "$index"
  )

  while IFS=$'\t' read -r component chart chart_version component_version \
    tag_reference image_index image_index_digest platform platform_image \
    platform_digest sbom sbom_sha; do
    local platform_slug="${platform//\//-}"
    local digest_slug="${platform_digest/:/-}"
    local expected_sbom="${component}-image-${component_version}-${platform_slug}-${digest_slug}.spdx.json"
    local source_name="${component}-image-${platform_slug}"
    local release_tag
    release_tag="$(jq -r '.release.tag' "$index")"
    local namespace="https://github.com/Azure/taugrid/sbom/${release_tag}/${expected_sbom}"
    test "$component_version" = "$chart_version" ||
      fail "$component component and chart versions differ"
    test "$component_version" = "${release_tag#v}" ||
      fail "$component version does not match $release_tag"
    [[ "$image_index_digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
      fail "$component has invalid image index digest"
    [[ "$platform_digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
      fail "$component $platform has invalid manifest digest"
    test "$image_index" = "${tag_reference%:*}@${image_index_digest}" ||
      fail "$component image index reference does not match its digest"
    test "$platform_image" = "${tag_reference%:*}@${platform_digest}" ||
      fail "$component $platform reference does not match its digest"
    test "$sbom" = "$expected_sbom" ||
      fail "unexpected image SBOM filename for $component $platform"
    test -f "$release_dir/$sbom" || fail "missing image SBOM $sbom"
    test "$(sha256_file "$release_dir/$sbom")" = "$sbom_sha" ||
      fail "image SBOM digest mismatch for $component $platform"
    validate_spdx "$release_dir/$sbom" "$source_name" "$namespace"
    case "$component:$chart:$platform" in
      tau:taugrid-core:linux/amd64 | tau:taugrid-core:linux/arm64 | \
        taugrid-portal:taugrid-core:linux/amd64 | \
        taugrid-portal:taugrid-core:linux/arm64 | \
        tau-core-controller:tau-core-controller:linux/amd64 | \
        tau-core-controller:tau-core-controller:linux/arm64) ;;
      *) fail "unexpected image release entry $component:$chart:$platform" ;;
    esac
  done < <(
    jq -r '.images[] | [
      .component, .chart, .chartVersion, .componentVersion, .tagReference,
      .imageIndex, .imageIndexDigest, .platform, .platformImage,
      .platformDigest, .sbom, .sbomSha256
    ] | @tsv' "$index"
  )
}

validate_release() {
  local release_dir="$1"
  local expected_file
  local actual_file
  expected_file="$(mktemp)"
  actual_file="$(mktemp)"
  validate_index "$release_dir"
  expected_assets "$release_dir" > "$expected_file"
  find "$release_dir" -maxdepth 1 -type f -exec basename {} \; |
    sort > "$actual_file"
  diff -u "$expected_file" "$actual_file" ||
    fail "release asset set does not match the SBOM index"
  rm -f "$expected_file" "$actual_file"
  (
    cd "$release_dir"
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum --check SHA256SUMS
    else
      shasum -a 256 --check SHA256SUMS
    fi
  )
}

build_index() {
  local release_dir="$1"
  local cli_metadata="$2"
  local image_metadata="$3"
  local release_tag="$4"
  local source_commit="$5"
  local generated_at="$6"
  jq -n \
    --arg schema "$SCHEMA_VERSION" \
    --arg tag "$release_tag" \
    --arg commit "$source_commit" \
    --arg generated "$generated_at" \
    --slurpfile cli "$cli_metadata" \
    --slurpfile images "$image_metadata" '{
      schemaVersion: $schema,
      release: {
        tag: $tag,
        sourceCommit: $commit,
        generatedAt: $generated
      },
      cli: ($cli | sort_by(.component, .target)),
      images: ($images | sort_by(.component, .platform))
    }' > "$release_dir/$INDEX_FILENAME"
  (
    cd "$release_dir"
    checksum_file="$(mktemp)"
    find . -maxdepth 1 -type f ! -name SHA256SUMS -print0 |
      sort -z |
      xargs -0 sha256sum |
      sed 's#  \\./#  #' > "$checksum_file"
    mv "$checksum_file" SHA256SUMS
  )
  validate_release "$release_dir"
}

case "${1:-}" in
  build)
    (( $# == 7 )) || fail "usage: $0 build RELEASE_DIR CLI_METADATA IMAGE_METADATA RELEASE_TAG SOURCE_COMMIT GENERATED_AT"
    build_index "$2" "$3" "$4" "$5" "$6" "$7"
    ;;
  validate)
    (( $# == 2 )) || fail "usage: $0 validate RELEASE_DIR"
    validate_release "$2"
    ;;
  expected-assets)
    (( $# == 2 )) || fail "usage: $0 expected-assets RELEASE_DIR"
    validate_index "$2"
    expected_assets "$2"
    ;;
  *)
    fail "usage: $0 {build|validate|expected-assets} ..."
    ;;
esac
