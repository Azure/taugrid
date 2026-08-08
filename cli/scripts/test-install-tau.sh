#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
installer="$script_dir/install-tau.sh"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/tau-installer-test.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT
stamped_installer="$tmp_dir/install.sh"
sed 's/@TAU_VERSION@/v1.2.3/g' "$installer" > "$stamped_installer"
chmod 0755 "$stamped_installer"

mock_bin="$tmp_dir/bin"
release_dir="$tmp_dir/release"
mkdir -p "$mock_bin" "$release_dir"
cp "$script_dir/../../LICENSE" "$release_dir/LICENSE"

cat > "$mock_bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) printf '%s\n' "$MOCK_UNAME_S" ;;
  -m) printf '%s\n' "$MOCK_UNAME_M" ;;
  *) exit 2 ;;
esac
EOF

cat > "$mock_bin/curl" <<'EOF'
#!/bin/sh
set -eu
output=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output | -o)
      output="$2"
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done
test -n "$output"
test -n "$url"
cp "$MOCK_RELEASE_DIR/${url##*/}" "$output"
EOF
chmod 0755 "$mock_bin/uname" "$mock_bin/curl"

write_binary() {
  asset="$1"
  cat > "$release_dir/$asset" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "version" ] && [ "${2:-}" = "--short" ]; then
  printf '%s\n' 'v1.2.3'
  exit 0
fi
exit 2
EOF
  chmod 0755 "$release_dir/$asset"
}

write_checksums() {
  (
    cd "$release_dir"
    shasum -a 256 tau-* LICENSE > SHA256SUMS
  )
}

run_stamped_case() {
  os_name="$1"
  machine="$2"
  asset="$3"
  install_dir="$tmp_dir/install-$os_name-$machine"
  license_dir="$tmp_dir/license-$os_name-$machine"

  MOCK_RELEASE_DIR="$release_dir" \
    MOCK_UNAME_M="$machine" \
    MOCK_UNAME_S="$os_name" \
    PATH="$mock_bin:$PATH" \
    TAU_INSTALL_DIR="$install_dir" \
    TAU_LICENSE_DIR="$license_dir" \
    "$stamped_installer" >/dev/null

  cmp "$release_dir/$asset" "$install_dir/tau"
  cmp "$release_dir/LICENSE" "$license_dir/LICENSE"
  test "$("$install_dir/tau" version --short)" = "v1.2.3"
}

write_binary "tau-linux-amd64"
write_binary "tau-darwin-arm64"
write_checksums

run_stamped_case "Linux" "x86_64" "tau-linux-amd64"
run_stamped_case "Darwin" "arm64" "tau-darwin-arm64"

printf '%064d  tau-linux-amd64\n' 0 > "$release_dir/SHA256SUMS"
bad_install_dir="$tmp_dir/install-bad-checksum"
if MOCK_RELEASE_DIR="$release_dir" \
  MOCK_UNAME_M="x86_64" \
  MOCK_UNAME_S="Linux" \
  PATH="$mock_bin:$PATH" \
  TAU_INSTALL_DIR="$bad_install_dir" \
  TAU_LICENSE_DIR="$tmp_dir/license-bad-checksum" \
  TAU_VERSION="v1.2.3" \
  "$installer" >/dev/null 2>&1; then
  echo "installer accepted a mismatched checksum" >&2
  exit 1
fi
test ! -e "$bad_install_dir/tau"

echo "tau installer tests passed"
