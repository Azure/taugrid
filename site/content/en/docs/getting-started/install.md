---
title: Install Tau
weight: 1
description: Install and verify the Tau CLI, then optionally add the Python SDK
---

{{< maturity status="ga" reviewed="2026-08-12" >}}

Tau consists of the Go `tau` CLI and an optional Python SDK package also
imported as `tau`. The CLI is the canonical Kubernetes executor.

## Install the current CLI

No GitHub Release is published. Install the CLI from source. Install Git and
the Go version declared in `cli/go.mod`, then run:

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid
make install-tau-cli

TAU_BIN_DIR="$(go env GOBIN)"
test -n "$TAU_BIN_DIR" || TAU_BIN_DIR="$(go env GOPATH)/bin"
export PATH="$TAU_BIN_DIR:$PATH"

command -v tau
tau version --short
tau --help >/dev/null
```

The Make target installs `tau` and `tau-gen` into `GOBIN`, or `GOPATH/bin` when
`GOBIN` is unset. It warns if another binary takes precedence on `PATH`. Add the
`export PATH=...` line to the shell startup file, such as `~/.profile`,
`~/.bashrc`, or `~/.zshrc`, so later terminals resolve the same binary.

A checkout build reports `dev` from `tau version --short`. `tau version` also
prints the source commit and build date. Pin and record the validated source
commit. To upgrade that installation:

```bash
git pull --ff-only
make install-tau-cli
tau version
```

## Install a published release

When a version is published on the
[GitHub Releases page](https://github.com/Azure/taugrid/releases), install that
specific version for a reproducible user environment:

```bash
TAU_VERSION="<published-vX.Y.Z>"
curl -fsSL \
  "https://github.com/Azure/taugrid/releases/download/$TAU_VERSION/install.sh" |
  TAU_VERSION="$TAU_VERSION" sh

export PATH="$HOME/.local/bin:$PATH"
command -v tau
tau version --short
```

Do not substitute an unpublished version. The release installer supports Linux
and macOS on amd64 and arm64, detects the current platform, downloads the
matching binary, verifies it against the release's `SHA256SUMS`, checks that the
binary reports the requested version, and only then installs it to
`$HOME/.local/bin` without sudo. It installs the MIT license to
`$HOME/.local/share/doc/tau/LICENSE`. Override these destinations with
`TAU_INSTALL_DIR` and `TAU_LICENSE_DIR`.

Run the command with a newer published `TAU_VERSION` to upgrade. Release assets
also include `tau-gen`. The installer installs only the researcher-facing
`tau` binary.

## Install the optional Python SDK

The Python SDK is optional. Only `tau python` and the `@tau.train()` decorator
need it. On Debian and Ubuntu (Python 3.11 and later) the system interpreter is
marked externally managed under [PEP 668](https://peps.python.org/pep-0668/) and
pip refuses to install into it. `make install-tau` reports that as a skip and
still installs the CLI. To install the SDK, use a virtual environment:

```bash
python3 -m venv ~/.venvs/tau
source ~/.venvs/tau/bin/activate
make -C sdk/python/python install
```

Or target an interpreter without activating it:

```bash
make -C sdk/python/python install PYTHON=~/.venvs/tau/bin/python
```

`tau python` resolves `python3` from `PATH`, so activate the virtual environment
before using it.

The Python SDK is installed from the same tagged source revision; releases do
not currently contain Python wheels.

See [`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md)
for the maintainer release checklist.
