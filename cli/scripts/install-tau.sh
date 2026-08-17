#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -eu

repository="${TAU_REPOSITORY:-Azure/taugrid}"
install_dir="${TAU_INSTALL_DIR:-${HOME:?HOME must be set}/.local/bin}"
license_dir="${TAU_LICENSE_DIR:-${HOME:?HOME must be set}/.local/share/doc/tau}"
default_version="@TAU_VERSION@"
version="${TAU_VERSION:-$default_version}"

github_cli_authenticated=false
if command -v gh >/dev/null 2>&1 &&
  gh auth status --hostname github.com >/dev/null 2>&1; then
  github_cli_authenticated=true
fi

if [ "$version" = "$default_version" ] &&
  [ "${default_version#@}" != "$default_version" ]; then
  if [ "$github_cli_authenticated" = true ]; then
    version="$(
      gh release view --repo "$repository" --json tagName --jq .tagName
    )"
  elif command -v curl >/dev/null 2>&1; then
    latest_url="$(
      curl -fsSL -o /dev/null -w '%{url_effective}' \
        "https://github.com/$repository/releases/latest"
    )"
    version="${latest_url##*/}"
  else
    echo "tau installer: authenticated gh or curl is required" >&2
    exit 1
  fi
fi

semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|([0-9]*[A-Za-z-][0-9A-Za-z-]*))(\.((0|[1-9][0-9]*)|([0-9]*[A-Za-z-][0-9A-Za-z-]*)))*)?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'
if ! printf '%s\n' "$version" | grep -Eq "$semver_re"; then
  echo "tau installer: invalid release version '$version'" >&2
  exit 1
fi

case "$(uname -s)" in
  Darwin)
    os="darwin"
    ;;
  Linux)
    os="linux"
    ;;
  *)
    echo "tau installer: unsupported operating system '$(uname -s)'" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64 | amd64)
    arch="amd64"
    ;;
  arm64 | aarch64)
    arch="arm64"
    ;;
  *)
    echo "tau installer: unsupported architecture '$(uname -m)'" >&2
    exit 1
    ;;
esac

asset="tau-$os-$arch"
release_url="https://github.com/$repository/releases/download/$version"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/tau-install.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

if [ "$github_cli_authenticated" = true ]; then
  gh release download "$version" \
    --repo "$repository" \
    --dir "$tmp_dir" \
    --pattern "$asset" \
    --pattern SHA256SUMS \
    --pattern LICENSE
elif command -v curl >/dev/null 2>&1; then
  curl -fsSL --output "$tmp_dir/$asset" "$release_url/$asset"
  curl -fsSL --output "$tmp_dir/SHA256SUMS" "$release_url/SHA256SUMS"
  curl -fsSL --output "$tmp_dir/LICENSE" "$release_url/LICENSE"
else
  echo "tau installer: authenticated gh or curl is required" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  checksum_command="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  checksum_command="shasum"
else
  echo "tau installer: sha256sum or shasum is required" >&2
  exit 1
fi

verify_checksum() {
  file="$1"
  expected_checksum="$(
    awk -v file="$file" '$2 == file { print $1 }' "$tmp_dir/SHA256SUMS"
  )"
  if [ "$(printf '%s\n' "$expected_checksum" | wc -l | tr -d ' ')" -ne 1 ] ||
    [ "${#expected_checksum}" -ne 64 ]; then
    echo "tau installer: SHA256SUMS has no unique checksum for $file" >&2
    exit 1
  fi

  if [ "$checksum_command" = "sha256sum" ]; then
    actual_checksum="$(sha256sum "$tmp_dir/$file" | awk '{ print $1 }')"
  else
    actual_checksum="$(shasum -a 256 "$tmp_dir/$file" | awk '{ print $1 }')"
  fi
  if [ "$actual_checksum" != "$expected_checksum" ]; then
    echo "tau installer: checksum verification failed for $file" >&2
    exit 1
  fi
}

verify_checksum "$asset"
verify_checksum "LICENSE"

chmod 0755 "$tmp_dir/$asset"
installed_version="$("$tmp_dir/$asset" version --short)"
if [ "$installed_version" != "$version" ]; then
  echo "tau installer: binary reports $installed_version, expected $version" >&2
  exit 1
fi

mkdir -p "$install_dir"
mkdir -p "$license_dir"
install -m 0755 "$tmp_dir/$asset" "$install_dir/tau"
install -m 0644 "$tmp_dir/LICENSE" "$license_dir/LICENSE"

echo "Installed tau $version to $install_dir/tau"
echo "Installed the MIT license to $license_dir/LICENSE"
case ":${PATH:-}:" in
  *":$install_dir:"*)
    ;;
  *)
    echo "Add $install_dir to PATH:"
    echo "  export PATH=\"$install_dir:\$PATH\""
    ;;
esac
