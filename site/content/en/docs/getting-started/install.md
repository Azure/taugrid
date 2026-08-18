---
title: Install Tau
weight: 1
description: Install and verify the Tau CLI, then optionally add the Python SDK
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

Tau consists of the Go `tau` CLI and an optional Python SDK package also
imported as `tau`. The CLI is the canonical Kubernetes executor.

## Install the latest release

The recommended installation downloads the latest stable
[GitHub Release](https://github.com/Azure/taugrid/releases) with `curl`. The
installer does not require `sudo`:

```bash
TAU_RELEASE_URL="$(
  curl -fsSL -o /dev/null -w '%{url_effective}' \
    https://github.com/Azure/taugrid/releases/latest
)"
TAU_VERSION="${TAU_RELEASE_URL##*/}"

curl -fsSL \
  "https://github.com/Azure/taugrid/releases/download/$TAU_VERSION/install.sh" |
  TAU_VERSION="$TAU_VERSION" bash

export PATH="$HOME/.local/bin:$PATH"
command -v tau
tau version --short
tau --help >/dev/null
```

`tau version --short` should print the same `vX.Y.Z` value stored in
`TAU_VERSION`. Add the `export PATH=...` line to the shell startup file, such as
`~/.profile`, `~/.bashrc`, or `~/.zshrc`, so later terminals resolve the same
binary.

The installer supports Linux and macOS on amd64 and arm64. It downloads the
matching binary, verifies it against the release's `SHA256SUMS`, checks that the
binary reports the requested version, and installs it to `$HOME/.local/bin`. It
also installs the MIT license to `$HOME/.local/share/doc/tau/LICENSE`. Override
these destinations with `TAU_INSTALL_DIR` and `TAU_LICENSE_DIR`.

Repeat the commands to upgrade to the newest stable release. For a reproducible
environment, set a published version explicitly instead of querying `latest`:

```bash
TAU_VERSION="<published-vX.Y.Z>"
curl -fsSL \
  "https://github.com/Azure/taugrid/releases/download/$TAU_VERSION/install.sh" |
  TAU_VERSION="$TAU_VERSION" bash
```

Do not substitute an unpublished version. Release assets also include
`tau-gen`, but `install.sh` installs only the researcher-facing `tau` binary.
The historical `v0.3.0` Release predates the release installer; check out that
tag with the advanced source path below or select a newer Release.

## Install the optional Python SDK

Only `tau python` and Python authoring APIs such as `@tau.train()` need the SDK.
Each new TauGrid Release contains one source-aligned `tau-*.whl` asset. The SDK
keeps its own package version, so discover the wheel name from the same Release
instead of constructing it from `TAU_VERSION`.

Use a virtual environment. This avoids the externally managed Python error from
[PEP 668](https://peps.python.org/pep-0668/) on current Debian and Ubuntu:

```bash
python3 -m venv ~/.venvs/tau
source ~/.venvs/tau/bin/activate

TAU_SDK_DIR="$(mktemp -d)"
TAU_RELEASE_JSON="$TAU_SDK_DIR/release.json"
curl -fsSL \
  "https://api.github.com/repos/Azure/taugrid/releases/tags/$TAU_VERSION" \
  -o "$TAU_RELEASE_JSON"

TAU_WHEEL_URL="$(
  python - "$TAU_RELEASE_JSON" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as release_file:
    release = json.load(release_file)

wheels = [
    asset["browser_download_url"]
    for asset in release["assets"]
    if asset["name"].startswith("tau-") and asset["name"].endswith(".whl")
]
if len(wheels) != 1:
    raise SystemExit(f"expected one SDK wheel, found {len(wheels)}")
print(wheels[0])
PY
)"
TAU_WHEEL_PATH="$TAU_SDK_DIR/${TAU_WHEEL_URL##*/}"
curl -fsSL "$TAU_WHEEL_URL" -o "$TAU_WHEEL_PATH"
python -m pip install "$TAU_WHEEL_PATH"
python -m pip show tau
python -m tau.cli --help >/dev/null
```

The commands assume `TAU_VERSION` is still set from the CLI installation. If you
start a new shell, rerun the latest-release query first. The release workflow
includes the wheel in `SHA256SUMS` and verifies the published digest before the
Release becomes visible. `tau python` resolves the Python interpreter from
`PATH`, so activate this virtual environment before using it.

Releases published before wheel packaging was added do not contain this asset.
For those historical releases, use the advanced source installation below or
select a newer release.

## Install from source (advanced)

Build from source only when developing TauGrid or testing an unpublished
commit. Install Git and the Go version declared in `cli/go.mod`, then run:

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid
# Optional: git checkout <tag-or-commit>
make install-tau-cli

TAU_BIN_DIR="$(go env GOBIN)"
test -n "$TAU_BIN_DIR" || TAU_BIN_DIR="$(go env GOPATH)/bin"
export PATH="$TAU_BIN_DIR:$PATH"

command -v tau
tau version --short
tau --help >/dev/null
```

The Make target installs `tau` and `tau-gen` into `GOBIN`, or `GOPATH/bin` when
`GOBIN` is unset. A checkout build reports `dev` from `tau version --short`;
`tau version` also prints the source commit and build date. Pin and record the
validated commit.

To install the SDK from the same checkout:

```bash
python3 -m venv ~/.venvs/tau
source ~/.venvs/tau/bin/activate
make -C sdk/python/python install
```

Or target an existing virtual environment without activating it:

```bash
make -C sdk/python/python install PYTHON=~/.venvs/tau/bin/python
```

See [`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md)
for the maintainer release checklist.
